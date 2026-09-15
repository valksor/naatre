package http

import (
	"io"

	"github.com/valksor/naatre/protocol/httpdigest"
)

const (
	DigestProfile                      = httpdigest.DigestProfile
	MaximumDigestFieldBytes            = httpdigest.MaximumDigestFieldBytes
	DefaultMaximumDigestBytes          = httpdigest.DefaultMaximumDigestBytes
	TrailerDigestVerificationSupported = httpdigest.TrailerDigestVerificationSupported
)

var (
	ErrDigestRequired             = httpdigest.ErrDigestRequired
	ErrMalformedDigest            = httpdigest.ErrMalformedDigest
	ErrUnsupportedDigestAlgorithm = httpdigest.ErrUnsupportedDigestAlgorithm
	ErrDuplicateDigestField       = httpdigest.ErrDuplicateDigestField
	ErrDuplicateDigestAlgorithm   = httpdigest.ErrDuplicateDigestAlgorithm
	ErrNoAcceptableDigest         = httpdigest.ErrNoAcceptableDigest
	ErrDigestMismatch             = httpdigest.ErrDigestMismatch
	ErrDigestLimitExceeded        = httpdigest.ErrDigestLimitExceeded
	ErrDigestTruncated            = httpdigest.ErrDigestTruncated
	ErrDigestLengthMismatch       = httpdigest.ErrDigestLengthMismatch
)

// DigestAlgorithm is an allowed RFC 9530 digest algorithm key.
type DigestAlgorithm = httpdigest.DigestAlgorithm

const (
	SHA512 = httpdigest.SHA512
	SHA256 = httpdigest.SHA256
)

// DigestValues contains every allowed digest value present in a field.
type DigestValues = httpdigest.DigestValues

// VerifyOptions controls bounded streaming verification.
type VerifyOptions = httpdigest.VerifyOptions

// FormatDigestField serializes a canonical Digest Fields dictionary.
func FormatDigestField(content []byte, algorithms ...DigestAlgorithm) (string, error) {
	return httpdigest.FormatDigestField(content, algorithms...)
}

// ParseDigestFields parses one singleton Content-Digest or Repr-Digest field.
func ParseDigestFields(fields []string) (DigestValues, error) {
	return httpdigest.ParseDigestFields(fields)
}

// NegotiateDigestAlgorithm selects the strongest acceptable profile algorithm.
func NegotiateDigestAlgorithm(fields []string) (DigestAlgorithm, error) {
	return httpdigest.NegotiateDigestAlgorithm(fields)
}

// VerifyDigestTo stages and hashes HTTP content and succeeds only after verified EOF.
func VerifyDigestTo(dst io.Writer, src io.Reader, fields []string, options VerifyOptions) (int64, error) {
	return httpdigest.VerifyDigestTo(dst, src, fields, options)
}
