package http

import (
	stdhttp "net/http"

	"github.com/valksor/naatre/protocol/httpdigest"
)

const (
	CodeDigestRequired                  = httpdigest.CodeDigestRequired
	CodeMalformedDigest                 = httpdigest.CodeMalformedDigest
	CodeUnsupportedDigestAlgorithm      = httpdigest.CodeUnsupportedDigestAlgorithm
	CodeDuplicateDigestField            = httpdigest.CodeDuplicateDigestField
	CodeDuplicateDigestAlgorithm        = httpdigest.CodeDuplicateDigestAlgorithm
	CodeNoAcceptableDigest              = httpdigest.CodeNoAcceptableDigest
	CodeDigestMismatch                  = httpdigest.CodeDigestMismatch
	CodeDigestLimitExceeded             = httpdigest.CodeDigestLimitExceeded
	CodeDigestTruncated                 = httpdigest.CodeDigestTruncated
	CodeDigestLengthMismatch            = httpdigest.CodeDigestLengthMismatch
	CodeDigestTrailerUnsupported        = httpdigest.CodeDigestTrailerUnsupported
	CodeDigestDowngrade                 = httpdigest.CodeDigestDowngrade
	CodeDigestContentCodingUnsupported  = httpdigest.CodeDigestContentCodingUnsupported
	CodeDigestRangeInvalid              = httpdigest.CodeDigestRangeInvalid
	CodeDigestRepresentationUnavailable = httpdigest.CodeDigestRepresentationUnavailable
	CodeDigestCanceled                  = httpdigest.CodeDigestCanceled
	CodeDigestIO                        = httpdigest.CodeDigestIO
	CodeDigestStreamUnsupported         = httpdigest.CodeDigestStreamUnsupported
)

const (
	DigestPhaseHeaders        = httpdigest.PhaseHeaders
	DigestPhaseContent        = httpdigest.PhaseContent
	DigestPhaseRepresentation = httpdigest.PhaseRepresentation
	DigestPhaseCompletion     = httpdigest.PhaseCompletion
)

var (
	ErrDigestTrailerUnsupported        = httpdigest.ErrDigestTrailerUnsupported
	ErrDigestDowngrade                 = httpdigest.ErrDigestDowngrade
	ErrDigestContentCodingUnsupported  = httpdigest.ErrDigestContentCodingUnsupported
	ErrDigestRangeInvalid              = httpdigest.ErrDigestRangeInvalid
	ErrDigestRepresentationUnavailable = httpdigest.ErrDigestRepresentationUnavailable
	ErrDigestCanceled                  = httpdigest.ErrDigestCanceled
	ErrDigestIO                        = httpdigest.ErrDigestIO
	ErrDigestStreamUnsupported         = httpdigest.ErrDigestStreamUnsupported
)

// DigestFailure is the safe public failure returned by HTTP integrity gates.
// It intentionally retains no body, header, credential, raw digest, or I/O
// implementation detail.
type DigestFailure struct {
	Code  string
	Phase string
}

func (failure *DigestFailure) Error() string {
	if failure == nil {
		return "http digest failure"
	}
	return "http digest: " + failure.Code + " at " + failure.Phase
}

// DigestErrorCode maps every profile failure to its stable public code.
func DigestErrorCode(err error) string {
	return httpdigest.ErrorCode(err)
}

// RequireDigestAlgorithm rejects a response that omits the algorithm selected
// by its corresponding Want field. All present allowed values are still
// verified by VerifyDigestTo.
func RequireDigestAlgorithm(fields []string, expected DigestAlgorithm) error {
	return httpdigest.RequireAlgorithm(fields, expected)
}

// RejectDigestTrailers enforces the core profile's initial-header-only
// boundary before protected content is consumed and again after EOF.
func RejectDigestTrailers(header, trailer stdhttp.Header) error {
	return httpdigest.RejectTrailers(header, trailer)
}

// DigestMode controls whether one digest field is outside the middleware,
// verified or emitted when present, or mandatory for the protected exchange.
type DigestMode uint8

const (
	DigestDisabled DigestMode = iota
	DigestOptional
	DigestRequired
)

// RangeRepresentationDigest supplies a trusted Repr-Digest for a 206 response.
// A range body cannot be relabelled as the complete selected representation.
// The origin callback must derive the value from that complete representation.
type RangeRepresentationDigest func(*stdhttp.Request, stdhttp.Header) (string, error)

// DigestMiddlewareConfig configures the router-independent net/http gate.
// Use CoreDigestMiddlewareConfig for the implemented core profile.
type DigestMiddlewareConfig struct {
	RequestContent         DigestMode
	RequestRepresentation  DigestMode
	ResponseContent        DigestMode
	ResponseRepresentation DigestMode

	MaximumRequestContentBytes         int64
	MaximumRequestRepresentationBytes  int64
	MaximumResponseContentBytes        int64
	MaximumResponseRepresentationBytes int64

	RangeRepresentationDigest RangeRepresentationDigest
}

// CoreDigestMiddlewareConfig requires both request fields and emits both
// response fields with bounded identity/gzip representation processing.
func CoreDigestMiddlewareConfig() DigestMiddlewareConfig {
	return DigestMiddlewareConfig{
		RequestContent: DigestRequired, RequestRepresentation: DigestRequired,
		ResponseContent: DigestRequired, ResponseRepresentation: DigestRequired,
		MaximumRequestContentBytes:         DefaultMaximumDigestBytes,
		MaximumRequestRepresentationBytes:  DefaultMaximumDigestBytes,
		MaximumResponseContentBytes:        DefaultMaximumDigestBytes,
		MaximumResponseRepresentationBytes: DefaultMaximumDigestBytes,
	}
}
