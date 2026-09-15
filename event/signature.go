package event

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// SignatureProfile pins the RFC 9421 component set and HMAC algorithm.
	SignatureProfile         = "naatre.webhook.rfc9421-1"
	SignatureLabel           = "naatre"
	SignatureAlgorithm       = "hmac-sha256"
	IdentityEncoding         = "identity"
	GZIPEncoding             = "gzip"
	DefaultMaximumBodyBytes  = 1 << 20
	MaximumSignatureValidity = 5 * time.Minute
)

var (
	ErrInvalidMessage  = errors.New("invalid webhook message")
	ErrDigest          = errors.New("webhook content digest failed")
	ErrSignature       = errors.New("webhook signature failed")
	ErrFreshness       = errors.New("webhook freshness check failed")
	ErrUnknownKey      = errors.New("webhook key is unknown")
	ErrRevokedKey      = errors.New("webhook key is revoked")
	ErrReplayStore     = errors.New("webhook replay store failed")
	keyIDPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	contentDigestExpr  = regexp.MustCompile(`^sha-256=:([A-Za-z0-9+/]{43}=):$`)
	signatureInputExpr = regexp.MustCompile(`^naatre=\("@method" "@target-uri" "content-type" "content-encoding" "content-digest" "naatre-webhook-id" "naatre-webhook-timestamp" "naatre-webhook-audience"\);created=([0-9]+);expires=([0-9]+);keyid="([A-Za-z0-9][A-Za-z0-9._:-]{0,127})";alg="hmac-sha256"$`)
	signatureExpr      = regexp.MustCompile(`^naatre=:([A-Za-z0-9+/]+={0,2}):$`)
)

var coveredComponents = []string{
	"@method", "@target-uri", "content-type", "content-encoding", "content-digest",
	"naatre-webhook-id", "naatre-webhook-timestamp", "naatre-webhook-audience",
}

// Message contains the exact HTTP message components covered by the pinned
// signature profile. Body is the content after transfer framing removal and
// before content-coding decompression.
type Message struct {
	Method          string
	TargetURI       string
	ContentType     string
	ContentEncoding string
	ContentDigest   string
	DeliveryID      string
	Timestamp       string
	Audience        string
	SignatureInput  string
	Signature       string
	Body            []byte
}

// SigningKey identifies one rotation generation. Overlapping secrets always
// use distinct key IDs so a receiver never tries multiple secrets implicitly.
type SigningKey struct {
	ID     string
	Secret []byte
}

// VerificationKey binds a key generation to one sender and endpoint audience.
type VerificationKey struct {
	ID        string
	Sender    string
	Audience  string
	Secret    []byte
	NotBefore time.Time
	NotAfter  time.Time
	Revoked   bool
}

// KeyResolver returns exactly one key generation for an exact key ID.
type KeyResolver func(keyID string) (VerificationKey, bool)

// ReplayStore atomically records a delivery ID within an endpoint audience.
// replay is true when the identifier was already recorded and unexpired.
type ReplayStore interface {
	CheckAndStore(audience, deliveryID string, now, expires time.Time) (replay bool, err error)
}

// VerifierConfig supplies identity, freshness, and replay policy.
type VerifierConfig struct {
	ResolveKey       KeyResolver
	ReplayStore      ReplayStore
	Now              func() time.Time
	ClockSkew        time.Duration
	MaximumValidity  time.Duration
	MaximumBodyBytes int64
}

// Verifier verifies the pinned exact-byte webhook profile.
type Verifier struct {
	resolveKey       KeyResolver
	replayStore      ReplayStore
	now              func() time.Time
	clockSkew        time.Duration
	maximumValidity  time.Duration
	maximumBodyBytes int64
}

// VerificationResult exposes authenticated identity, freshness, and replay
// status without exposing secret, signature, digest, or payload bytes.
type VerificationResult struct {
	Sender     string
	KeyID      string
	Audience   string
	DeliveryID string
	Created    time.Time
	Expires    time.Time
	Replay     bool
}

// NewVerifier constructs a fail-closed receiver verifier.
func NewVerifier(config VerifierConfig) (*Verifier, error) {
	if config.ResolveKey == nil || config.ReplayStore == nil || config.ClockSkew < 0 ||
		config.MaximumValidity <= 0 || config.MaximumValidity > MaximumSignatureValidity {
		return nil, errors.New("webhook verifier requires key, replay, and freshness policy")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.MaximumBodyBytes == 0 {
		config.MaximumBodyBytes = DefaultMaximumBodyBytes
	}
	if config.MaximumBodyBytes < 1 {
		return nil, errors.New("webhook verifier body limit must be positive")
	}
	return &Verifier{
		resolveKey: config.ResolveKey, replayStore: config.ReplayStore, now: config.Now,
		clockSkew: config.ClockSkew, maximumValidity: config.MaximumValidity, maximumBodyBytes: config.MaximumBodyBytes,
	}, nil
}

// Sign computes Content-Digest and RFC 9421 fields over the exact message.
func Sign(message Message, key SigningKey, created, expires time.Time) (Message, error) {
	if !keyIDPattern.MatchString(key.ID) || len(key.Secret) < 32 || !validUnsignedMessage(message) ||
		!validWindow(created, expires) || expires.Sub(created) > MaximumSignatureValidity {
		return Message{}, ErrInvalidMessage
	}
	digest := sha256.Sum256(message.Body)
	message.ContentDigest = "sha-256=:" + base64.StdEncoding.EncodeToString(digest[:]) + ":"
	message.Timestamp = strconv.FormatInt(created.Unix(), 10)
	message.SignatureInput = formatSignatureInput(created.Unix(), expires.Unix(), key.ID)
	base := signatureBase(message, message.SignatureInput)
	mac := hmac.New(sha256.New, key.Secret)
	_, _ = mac.Write(base)
	message.Signature = SignatureLabel + "=:" + base64.StdEncoding.EncodeToString(mac.Sum(nil)) + ":"
	return message, nil
}

// Verify authenticates the digest and sender before atomically determining
// replay status. A duplicate is a successful authenticated result with Replay
// set; applications can acknowledge it without repeating its effect.
func (v *Verifier) Verify(message Message) (VerificationResult, error) {
	if v == nil || !validSignedMessage(message) || int64(len(message.Body)) > v.maximumBodyBytes {
		return VerificationResult{}, ErrInvalidMessage
	}
	createdUnix, expiresUnix, keyID, ok := parseSignatureInput(message.SignatureInput)
	if !ok || message.Timestamp != strconv.FormatInt(createdUnix, 10) {
		return VerificationResult{}, ErrSignature
	}
	created, expires := time.Unix(createdUnix, 0), time.Unix(expiresUnix, 0)
	now := v.now()
	if !validWindow(created, expires) || expires.Sub(created) > v.maximumValidity || now.Before(created.Add(-v.clockSkew)) || !now.Before(expires) {
		return VerificationResult{}, ErrFreshness
	}
	key, found := v.resolveKey(keyID)
	if !found || key.ID != keyID {
		return VerificationResult{}, ErrUnknownKey
	}
	if key.Revoked || message.Audience != key.Audience || !boundedText(key.Sender, 256) || len(key.Secret) < 32 || created.Before(key.NotBefore) || expires.After(key.NotAfter) {
		return VerificationResult{}, ErrRevokedKey
	}
	if !verifyContentDigest(message.Body, message.ContentDigest) {
		return VerificationResult{}, ErrDigest
	}
	signature, ok := parseSignature(message.Signature)
	if !ok {
		return VerificationResult{}, ErrSignature
	}
	mac := hmac.New(sha256.New, key.Secret)
	_, _ = mac.Write(signatureBase(message, message.SignatureInput))
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return VerificationResult{}, ErrSignature
	}
	replay, err := v.replayStore.CheckAndStore(message.Audience, message.DeliveryID, now, expires)
	if err != nil {
		return VerificationResult{}, ErrReplayStore
	}
	return VerificationResult{
		Sender: key.Sender, KeyID: key.ID, Audience: message.Audience, DeliveryID: message.DeliveryID,
		Created: created, Expires: expires, Replay: replay,
	}, nil
}

// MemoryReplayStore is a concurrency-safe reference store for tests and
// single-process receivers. Durable deployments use a shared atomic store.
type MemoryReplayStore struct {
	mu      sync.Mutex
	expires map[string]time.Time
}

// CheckAndStore implements ReplayStore.
func (s *MemoryReplayStore) CheckAndStore(audience, deliveryID string, now, expires time.Time) (bool, error) {
	if s == nil || !boundedText(audience, 256) || !boundedText(deliveryID, 128) || now.IsZero() || !expires.After(now) {
		return false, ErrInvalidMessage
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.expires == nil {
		s.expires = make(map[string]time.Time)
	}
	key := audience + "\x00" + deliveryID
	if recordedUntil, ok := s.expires[key]; ok && recordedUntil.After(now) {
		return true, nil
	}
	s.expires[key] = expires
	return false, nil
}

func validUnsignedMessage(message Message) bool {
	if message.Method != "POST" || !validWebhookTarget(message.TargetURI) || !boundedText(message.DeliveryID, 128) || !boundedText(message.Audience, 256) {
		return false
	}
	if message.ContentType != JSONContentType && message.ContentType != BatchJSONContentType {
		return false
	}
	return (message.ContentEncoding == IdentityEncoding || message.ContentEncoding == GZIPEncoding) && len(message.Body) > 0
}

func validSignedMessage(message Message) bool {
	return validUnsignedMessage(message) && message.ContentDigest != "" && message.Timestamp != "" && message.SignatureInput != "" && message.Signature != ""
}

func validWebhookTarget(target string) bool {
	parsed, err := url.Parse(target)
	return err == nil && parsed.Scheme == "https" && parsed.Hostname() != "" && parsed.User == nil && parsed.Fragment == ""
}

func validWindow(created, expires time.Time) bool {
	return !created.IsZero() && !expires.IsZero() && created.Nanosecond() == 0 && expires.Nanosecond() == 0 && expires.After(created)
}

func formatSignatureInput(created, expires int64, keyID string) string {
	return SignatureLabel + "=(\"" + strings.Join(coveredComponents, "\" \"") + "\");created=" + strconv.FormatInt(created, 10) +
		";expires=" + strconv.FormatInt(expires, 10) + ";keyid=\"" + keyID + "\";alg=\"" + SignatureAlgorithm + "\""
}

func signatureBase(message Message, signatureInput string) []byte {
	lines := []string{
		`"@method": ` + message.Method,
		`"@target-uri": ` + message.TargetURI,
		`"content-type": ` + message.ContentType,
		`"content-encoding": ` + message.ContentEncoding,
		`"content-digest": ` + message.ContentDigest,
		`"naatre-webhook-id": ` + message.DeliveryID,
		`"naatre-webhook-timestamp": ` + message.Timestamp,
		`"naatre-webhook-audience": ` + message.Audience,
		`"@signature-params": ` + strings.TrimPrefix(signatureInput, SignatureLabel+"="),
	}
	return []byte(strings.Join(lines, "\n"))
}

func parseSignatureInput(value string) (int64, int64, string, bool) {
	match := signatureInputExpr.FindStringSubmatch(value)
	if len(match) != 4 {
		return 0, 0, "", false
	}
	created, createdErr := strconv.ParseInt(match[1], 10, 64)
	expires, expiresErr := strconv.ParseInt(match[2], 10, 64)
	return created, expires, match[3], createdErr == nil && expiresErr == nil
}

func parseSignature(value string) ([]byte, bool) {
	match := signatureExpr.FindStringSubmatch(value)
	if len(match) != 2 {
		return nil, false
	}
	decoded, err := base64.StdEncoding.DecodeString(match[1])
	return decoded, err == nil && len(decoded) == sha256.Size && base64.StdEncoding.EncodeToString(decoded) == match[1]
}

func verifyContentDigest(body []byte, field string) bool {
	match := contentDigestExpr.FindStringSubmatch(field)
	if len(match) != 2 {
		return false
	}
	expected, err := base64.StdEncoding.DecodeString(match[1])
	actual := sha256.Sum256(body)
	return err == nil && len(expected) == sha256.Size && hmac.Equal(expected, actual[:])
}
