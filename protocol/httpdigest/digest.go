// Package httpdigest implements the core.http.digest-1 digest field profile.
// It is the shared inward dependency for HTTP transports and SDK clients.
package httpdigest

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"slices"
	"strings"
)

const (
	// DigestProfile is Naatre's pinned RFC 9530 and RFC 9651 profile.
	DigestProfile = "core.http.digest-1"
	// MaximumDigestFieldBytes bounds parsing before any digest value is decoded.
	MaximumDigestFieldBytes = 4096
	// DefaultMaximumDigestBytes bounds streaming verification when no smaller
	// application limit is supplied.
	DefaultMaximumDigestBytes = 1 << 20
	// TrailerDigestVerificationSupported is false because the core profile
	// requires digest fields in the header section.
	TrailerDigestVerificationSupported = false
)

var (
	ErrDigestRequired             = errors.New("digest field is required")
	ErrMalformedDigest            = errors.New("malformed digest field")
	ErrUnsupportedDigestAlgorithm = errors.New("unsupported digest algorithm")
	ErrDuplicateDigestField       = errors.New("duplicate digest field")
	ErrDuplicateDigestAlgorithm   = errors.New("duplicate digest algorithm")
	ErrNoAcceptableDigest         = errors.New("no acceptable digest algorithm")
	ErrDigestMismatch             = errors.New("digest mismatch")
	ErrDigestLimitExceeded        = errors.New("digest content limit exceeded")
	ErrDigestTruncated            = errors.New("digest content is truncated")
	ErrDigestLengthMismatch       = errors.New("digest content length mismatch")
)

// DigestAlgorithm is an allowed RFC 9530 digest algorithm key.
type DigestAlgorithm string

const (
	SHA512 DigestAlgorithm = "sha-512"
	SHA256 DigestAlgorithm = "sha-256"
)

var algorithmPreference = []DigestAlgorithm{SHA512, SHA256}

// DigestValues contains all digest values present in a field. Callers must not
// select a single value and ignore the rest: VerifyDigestTo verifies every
// present allowed value.
type DigestValues map[DigestAlgorithm][]byte

// VerifyOptions controls bounded streaming verification. ExpectedLength is an
// exact HTTP content length; use -1 when no independently established length
// is available. A zero MaximumBytes resolves to DefaultMaximumDigestBytes.
type VerifyOptions struct {
	MaximumBytes   int64
	ExpectedLength int64
}

// FormatDigestField serializes a canonical Digest Fields dictionary. With no
// algorithms it emits the profile default, sha-256.
func FormatDigestField(content []byte, algorithms ...DigestAlgorithm) (string, error) {
	if len(algorithms) == 0 {
		algorithms = []DigestAlgorithm{SHA256}
	}
	requested := make(map[DigestAlgorithm]bool, len(algorithms))
	for _, algorithm := range algorithms {
		if !supportedAlgorithm(algorithm) {
			return "", fmt.Errorf("%w: %q", ErrUnsupportedDigestAlgorithm, algorithm)
		}
		if requested[algorithm] {
			return "", fmt.Errorf("%w: %s", ErrDuplicateDigestAlgorithm, algorithm)
		}
		requested[algorithm] = true
	}
	parts := make([]string, 0, len(requested))
	for _, algorithm := range algorithmPreference {
		if !requested[algorithm] {
			continue
		}
		digest, _ := digestBytes(algorithm, content)
		parts = append(parts, string(algorithm)+"=:"+base64.StdEncoding.EncodeToString(digest)+":")
	}
	return strings.Join(parts, ", "), nil
}

// ParseDigestFields parses one singleton Content-Digest or Repr-Digest field.
// Multiple field lines are forbidden by the profile even if they could be
// combined as an RFC 9651 dictionary.
func ParseDigestFields(fields []string) (DigestValues, error) {
	switch len(fields) {
	case 0:
		return nil, ErrDigestRequired
	case 1:
		return parseDigestField(fields[0])
	default:
		return nil, ErrDuplicateDigestField
	}
}

// NegotiateDigestAlgorithm parses one Want-Content-Digest or Want-Repr-Digest
// field. Values are RFC 9530 weights from 0 through 10. An absent field selects
// sha-256. Equal positive weights use the server preference sha-512, sha-256.
func NegotiateDigestAlgorithm(fields []string) (DigestAlgorithm, error) {
	if len(fields) == 0 {
		return SHA256, nil
	}
	if len(fields) > 1 {
		return "", ErrDuplicateDigestField
	}
	weights, err := parseWantDigestField(fields[0])
	if err != nil {
		return "", err
	}
	bestWeight := 0
	var best DigestAlgorithm
	for _, algorithm := range algorithmPreference {
		if weight := weights[algorithm]; weight > bestWeight {
			best, bestWeight = algorithm, weight
		}
	}
	if best == "" {
		return "", ErrNoAcceptableDigest
	}
	return best, nil
}

// VerifyDigestTo copies HTTP content bytes to an untrusted staging writer while
// hashing them. It returns success only after verified EOF, so protected use,
// finalization, cache insertion, and client success can be gated on nil error.
// At most MaximumBytes+1 bytes are read and staged.
func VerifyDigestTo(dst io.Writer, src io.Reader, fields []string, options VerifyOptions) (int64, error) {
	values, err := ParseDigestFields(fields)
	if err != nil {
		return 0, err
	}
	if dst == nil || src == nil || options.MaximumBytes < 0 || options.MaximumBytes == math.MaxInt64 || options.ExpectedLength < -1 {
		return 0, fmt.Errorf("%w: invalid verification options", ErrMalformedDigest)
	}
	maximum := options.MaximumBytes
	if maximum == 0 {
		maximum = DefaultMaximumDigestBytes
	}
	if options.ExpectedLength > maximum {
		return 0, ErrDigestLimitExceeded
	}
	readLimit := maximum
	if options.ExpectedLength >= 0 {
		readLimit = options.ExpectedLength
	}

	hashers, writers := digestWriters(dst, values)
	buffer := make([]byte, 32*1024)
	n, copyErr := io.CopyBuffer(io.MultiWriter(writers...), io.LimitReader(src, readLimit+1), buffer)
	if copyErr != nil {
		return n, fmt.Errorf("verify digest stream: %w", copyErr)
	}
	if n > maximum {
		return n, ErrDigestLimitExceeded
	}
	if options.ExpectedLength >= 0 && n < options.ExpectedLength {
		return n, ErrDigestTruncated
	}
	if options.ExpectedLength >= 0 && n > options.ExpectedLength {
		return n, ErrDigestLengthMismatch
	}
	if err := compareDigestValues(hashers, values); err != nil {
		return n, err
	}
	return n, nil
}

func parseDigestField(value string) (DigestValues, error) {
	members, err := parseDictionaryMembers(value)
	if err != nil {
		return nil, err
	}
	values := make(DigestValues, len(members))
	for _, member := range members {
		if len(member.value) < 2 || member.value[0] != ':' || member.value[len(member.value)-1] != ':' || strings.Count(member.value, ":") != 2 {
			return nil, ErrMalformedDigest
		}
		encoded := member.value[1 : len(member.value)-1]
		decoded, decodeErr := base64.StdEncoding.Strict().DecodeString(encoded)
		if decodeErr != nil || base64.StdEncoding.EncodeToString(decoded) != encoded || len(decoded) != digestSize(member.algorithm) {
			return nil, ErrMalformedDigest
		}
		values[member.algorithm] = decoded
	}
	return values, nil
}

func parseWantDigestField(value string) (map[DigestAlgorithm]int, error) {
	members, err := parseDictionaryMembers(value)
	if err != nil {
		return nil, err
	}
	weights := make(map[DigestAlgorithm]int, len(members))
	for _, member := range members {
		if (len(member.value) != 1 || member.value[0] < '0' || member.value[0] > '9') && member.value != "10" {
			return nil, ErrMalformedDigest
		}
		if member.value == "10" {
			weights[member.algorithm] = 10
		} else {
			weights[member.algorithm] = int(member.value[0] - '0')
		}
	}
	return weights, nil
}

type dictionaryMember struct {
	algorithm DigestAlgorithm
	value     string
}

func parseDictionaryMembers(value string) ([]dictionaryMember, error) {
	rawMembers, err := splitDictionary(value)
	if err != nil {
		return nil, err
	}
	members := make([]dictionaryMember, 0, len(rawMembers))
	seen := make(map[DigestAlgorithm]bool, len(rawMembers))
	for _, rawMember := range rawMembers {
		key, rawValue, ok := strings.Cut(rawMember, "=")
		algorithm := DigestAlgorithm(key)
		if !ok || key == "" || !supportedAlgorithm(algorithm) {
			if ok && validDictionaryKey(key) {
				return nil, fmt.Errorf("%w: %q", ErrUnsupportedDigestAlgorithm, key)
			}
			return nil, ErrMalformedDigest
		}
		if seen[algorithm] {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateDigestAlgorithm, algorithm)
		}
		seen[algorithm] = true
		members = append(members, dictionaryMember{algorithm: algorithm, value: rawValue})
	}
	return members, nil
}

func digestWriters(dst io.Writer, values DigestValues) (map[DigestAlgorithm]hash.Hash, []io.Writer) {
	hashers := make(map[DigestAlgorithm]hash.Hash, len(values))
	writers := []io.Writer{dst}
	for _, algorithm := range algorithmPreference {
		if _, present := values[algorithm]; present {
			hasher, _ := newDigestHash(algorithm)
			hashers[algorithm] = hasher
			writers = append(writers, hasher)
		}
	}
	return hashers, writers
}

func compareDigestValues(hashers map[DigestAlgorithm]hash.Hash, values DigestValues) error {
	for _, algorithm := range algorithmPreference {
		expected, present := values[algorithm]
		if present && !hmac.Equal(hashers[algorithm].Sum(nil), expected) {
			return fmt.Errorf("%w: %s", ErrDigestMismatch, algorithm)
		}
	}
	return nil
}

func splitDictionary(value string) ([]string, error) {
	if len(value) == 0 || len(value) > MaximumDigestFieldBytes || strings.ContainsAny(value, "\r\n") {
		return nil, ErrMalformedDigest
	}
	rawMembers := strings.Split(value, ",")
	if len(rawMembers) == 0 || len(rawMembers) > len(algorithmPreference) {
		return nil, ErrMalformedDigest
	}
	members := make([]string, 0, len(rawMembers))
	for _, raw := range rawMembers {
		member := strings.Trim(raw, " \t")
		if member == "" || strings.ContainsAny(member, " \t") {
			return nil, ErrMalformedDigest
		}
		members = append(members, member)
	}
	return members, nil
}

func validDictionaryKey(value string) bool {
	const initial = "abcdefghijklmnopqrstuvwxyz"
	const remainder = initial + "0123456789_.*-"
	if value == "" || !strings.ContainsRune(initial, rune(value[0])) {
		return false
	}
	for _, character := range value[1:] {
		if !strings.ContainsRune(remainder, character) {
			return false
		}
	}
	return true
}

func supportedAlgorithm(algorithm DigestAlgorithm) bool {
	return slices.Contains(algorithmPreference, algorithm)
}

func digestSize(algorithm DigestAlgorithm) int {
	switch algorithm {
	case SHA512:
		return sha512.Size
	case SHA256:
		return sha256.Size
	default:
		return 0
	}
}

func newDigestHash(algorithm DigestAlgorithm) (hash.Hash, error) {
	switch algorithm {
	case SHA512:
		return sha512.New(), nil
	case SHA256:
		return sha256.New(), nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedDigestAlgorithm, algorithm)
	}
}

func digestBytes(algorithm DigestAlgorithm, content []byte) ([]byte, error) {
	hasher, err := newDigestHash(algorithm)
	if err != nil {
		return nil, err
	}
	_, _ = hasher.Write(content)
	return hasher.Sum(nil), nil
}
