package httpdigest

import (
	"context"
	"errors"
	"mime"
	"net/http"
	"strconv"
	"strings"
)

const (
	CodeDigestRequired                  = "DIGEST_REQUIRED"
	CodeMalformedDigest                 = "MALFORMED_DIGEST"
	CodeUnsupportedDigestAlgorithm      = "UNSUPPORTED_DIGEST_ALGORITHM"
	CodeDuplicateDigestField            = "DUPLICATE_DIGEST_FIELD"
	CodeDuplicateDigestAlgorithm        = "DUPLICATE_DIGEST_ALGORITHM"
	CodeNoAcceptableDigest              = "NO_ACCEPTABLE_DIGEST"
	CodeDigestMismatch                  = "DIGEST_MISMATCH"
	CodeDigestLimitExceeded             = "DIGEST_LIMIT_EXCEEDED"
	CodeDigestTruncated                 = "DIGEST_TRUNCATED"
	CodeDigestLengthMismatch            = "DIGEST_LENGTH_MISMATCH"
	CodeDigestTrailerUnsupported        = "DIGEST_TRAILER_UNSUPPORTED"
	CodeDigestDowngrade                 = "DIGEST_DOWNGRADE"
	CodeDigestContentCodingUnsupported  = "DIGEST_CONTENT_CODING_UNSUPPORTED"
	CodeDigestRangeInvalid              = "DIGEST_RANGE_INVALID"
	CodeDigestRepresentationUnavailable = "DIGEST_REPRESENTATION_UNAVAILABLE"
	CodeDigestCanceled                  = "DIGEST_CANCELED"
	CodeDigestIO                        = "DIGEST_IO"
	CodeDigestStreamUnsupported         = "DIGEST_STREAM_UNSUPPORTED"
)

const (
	PhaseHeaders        = "headers"
	PhaseContent        = "content"
	PhaseRepresentation = "representation"
	PhaseCompletion     = "completion"
)

var (
	ErrDigestTrailerUnsupported        = errors.New("digest trailers are unsupported")
	ErrDigestDowngrade                 = errors.New("digest negotiation was downgraded")
	ErrDigestContentCodingUnsupported  = errors.New("digest content coding is unsupported")
	ErrDigestRangeInvalid              = errors.New("digest range metadata is invalid")
	ErrDigestRepresentationUnavailable = errors.New("digest representation is unavailable")
	ErrDigestCanceled                  = errors.New("digest verification was canceled")
	ErrDigestIO                        = errors.New("digest verification I/O failed")
	ErrDigestStreamUnsupported         = errors.New("digest verification cannot commit an indefinite stream")
)

// ErrorCode maps every profile failure to its stable public code.
func ErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrDigestRequired):
		return CodeDigestRequired
	case errors.Is(err, ErrMalformedDigest):
		return CodeMalformedDigest
	case errors.Is(err, ErrUnsupportedDigestAlgorithm):
		return CodeUnsupportedDigestAlgorithm
	case errors.Is(err, ErrDuplicateDigestField):
		return CodeDuplicateDigestField
	case errors.Is(err, ErrDuplicateDigestAlgorithm):
		return CodeDuplicateDigestAlgorithm
	case errors.Is(err, ErrNoAcceptableDigest):
		return CodeNoAcceptableDigest
	case errors.Is(err, ErrDigestMismatch):
		return CodeDigestMismatch
	case errors.Is(err, ErrDigestLimitExceeded):
		return CodeDigestLimitExceeded
	case errors.Is(err, ErrDigestTruncated):
		return CodeDigestTruncated
	case errors.Is(err, ErrDigestLengthMismatch):
		return CodeDigestLengthMismatch
	case errors.Is(err, ErrDigestTrailerUnsupported):
		return CodeDigestTrailerUnsupported
	case errors.Is(err, ErrDigestDowngrade):
		return CodeDigestDowngrade
	case errors.Is(err, ErrDigestContentCodingUnsupported):
		return CodeDigestContentCodingUnsupported
	case errors.Is(err, ErrDigestRangeInvalid):
		return CodeDigestRangeInvalid
	case errors.Is(err, ErrDigestRepresentationUnavailable):
		return CodeDigestRepresentationUnavailable
	case errors.Is(err, ErrDigestCanceled), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return CodeDigestCanceled
	case errors.Is(err, ErrDigestStreamUnsupported):
		return CodeDigestStreamUnsupported
	default:
		return CodeDigestIO
	}
}

// RequireAlgorithm rejects a digest field that omits the selected algorithm.
// Every present allowed value must still be verified by VerifyDigestTo.
func RequireAlgorithm(fields []string, expected DigestAlgorithm) error {
	values, err := ParseDigestFields(fields)
	if err != nil {
		return err
	}
	if !supportedAlgorithm(expected) {
		return ErrUnsupportedDigestAlgorithm
	}
	if _, present := values[expected]; !present {
		return ErrDigestDowngrade
	}
	return nil
}

// RejectTrailers enforces the profile's initial-header-only digest boundary.
func RejectTrailers(header, trailer http.Header) error {
	for _, line := range header.Values("Trailer") {
		for _, name := range strings.Split(line, ",") {
			if digestFieldName(name) {
				return ErrDigestTrailerUnsupported
			}
		}
	}
	for name := range trailer {
		if digestFieldName(name) {
			return ErrDigestTrailerUnsupported
		}
	}
	return nil
}

// ValidateContentEncoding accepts the representation codings executed by the
// Go implementation profile without reading body bytes.
func ValidateContentEncoding(header http.Header) error {
	values := header.Values("Content-Encoding")
	if len(values) > 1 {
		return ErrDigestContentCodingUnsupported
	}
	encoding := strings.ToLower(strings.TrimSpace(header.Get("Content-Encoding")))
	if encoding != "" && encoding != "identity" && encoding != "gzip" {
		return ErrDigestContentCodingUnsupported
	}
	return nil
}

// ValidateResponseRange prevents range content from being mistaken for proof
// of a complete selected representation.
func ValidateResponseRange(status int, header http.Header, contentBytes int64) error {
	values := header.Values("Content-Range")
	if status != http.StatusPartialContent {
		if len(values) != 0 {
			return ErrDigestRangeInvalid
		}
		return nil
	}
	mediaType, _, _ := mime.ParseMediaType(header.Get("Content-Type"))
	if strings.EqualFold(mediaType, "multipart/byteranges") {
		if len(values) != 0 {
			return ErrDigestRangeInvalid
		}
		return nil
	}
	if len(values) != 1 {
		return ErrDigestRangeInvalid
	}
	value := strings.TrimSpace(values[0])
	if !strings.HasPrefix(value, "bytes ") {
		return ErrDigestRangeInvalid
	}
	rangePart, sizePart, ok := strings.Cut(strings.TrimPrefix(value, "bytes "), "/")
	startPart, endPart, rangeOK := strings.Cut(rangePart, "-")
	start, startErr := strconv.ParseInt(startPart, 10, 64)
	end, endErr := strconv.ParseInt(endPart, 10, 64)
	size, sizeErr := strconv.ParseInt(sizePart, 10, 64)
	if !ok || !rangeOK || startErr != nil || endErr != nil || sizeErr != nil || start < 0 || end < start || size <= end || end-start+1 != contentBytes {
		return ErrDigestRangeInvalid
	}
	return nil
}

func digestFieldName(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), "Content-Digest") || strings.EqualFold(strings.TrimSpace(name), "Repr-Digest")
}
