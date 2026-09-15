package event

import (
	"errors"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"
)

var (
	ErrInvalidEndpoint   = errors.New("invalid webhook endpoint")
	ErrDeliveryOwnership = errors.New("webhook delivery ownership mismatch")
	nonPublicPrefixes    = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"),
		netip.MustParsePrefix("2001:db8::/32"),
	}
)

// EndpointStatus controls whether new attempts may use a verified endpoint.
type EndpointStatus string

const (
	EndpointPending EndpointStatus = "pending-verification"
	EndpointActive  EndpointStatus = "active"
	EndpointRevoked EndpointStatus = "revoked"
)

// OrderingMode explicitly declares the delivery ordering contract.
type OrderingMode string

const (
	OrderingAbsent OrderingMode = "absent"
	OrderingStream OrderingMode = "event-stream"
)

// EndpointRegistration is the authorization-owned, DNS-pinned registration
// record consumed by delivery adapters. ResolvedAddresses must be revalidated
// for every connection to prevent DNS rebinding.
type EndpointRegistration struct {
	ID                string
	Tenant            string
	Revision          string
	URL               string
	Status            EndpointStatus
	Ordering          OrderingMode
	AllowedEventTypes []string
	ResolvedAddresses []netip.Addr
}

// ValidateEndpointRegistration checks the portable SSRF and ownership inputs.
// It performs no DNS lookup and never creates a network connection.
func ValidateEndpointRegistration(registration EndpointRegistration) error {
	if !validEndpointIdentity(registration) || !validWebhookTarget(registration.URL) ||
		!validEndpointAddresses(registration.URL, registration.ResolvedAddresses) || !validAllowedEventTypes(registration.AllowedEventTypes) {
		return ErrInvalidEndpoint
	}
	return nil
}

func validEndpointIdentity(registration EndpointRegistration) bool {
	return boundedText(registration.ID, 128) && boundedText(registration.Tenant, 128) && boundedText(registration.Revision, 128) &&
		(registration.Status == EndpointPending || registration.Status == EndpointActive || registration.Status == EndpointRevoked) &&
		(registration.Ordering == OrderingAbsent || registration.Ordering == OrderingStream)
}

func validEndpointAddresses(target string, addresses []netip.Addr) bool {
	if len(addresses) == 0 || len(addresses) > 8 {
		return false
	}
	seen := make(map[netip.Addr]struct{}, len(addresses))
	for _, address := range addresses {
		address = address.Unmap()
		if !publicAddress(address) {
			return false
		}
		if _, exists := seen[address]; exists {
			return false
		}
		seen[address] = struct{}{}
	}
	parsed, err := url.Parse(target)
	if err != nil {
		return false
	}
	literal, err := netip.ParseAddr(parsed.Hostname())
	if err != nil {
		return true
	}
	_, matches := seen[literal.Unmap()]
	return matches && len(seen) == 1
}

func validAllowedEventTypes(values []string) bool {
	if len(values) == 0 || len(values) > 256 {
		return false
	}
	types := slices.Clone(values)
	slices.Sort(types)
	for index, eventType := range types {
		if !eventTypePattern.MatchString(eventType) || index > 0 && eventType == types[index-1] {
			return false
		}
	}
	return true
}

// RetryPolicy defines bounded exponential backoff. Attempt numbers are
// one-based; DelayBefore returns the delay before the named retry attempt.
type RetryPolicy struct {
	MaximumAttempts uint32
	InitialDelay    time.Duration
	MaximumDelay    time.Duration
	Multiplier      uint32
}

// DelayBefore returns a bounded deterministic delay before attempt >= 2.
// Jitter is adapter-owned and MUST NOT reduce this lower bound.
func (p RetryPolicy) DelayBefore(attempt uint32) (time.Duration, error) {
	if p.MaximumAttempts < 1 || p.InitialDelay <= 0 || p.MaximumDelay < p.InitialDelay || p.Multiplier < 2 || attempt < 2 || attempt > p.MaximumAttempts {
		return 0, errors.New("invalid webhook retry policy or attempt")
	}
	delay := p.InitialDelay
	for current := uint32(2); current < attempt; current++ {
		if delay >= p.MaximumDelay/time.Duration(p.Multiplier) {
			return p.MaximumDelay, nil
		}
		delay *= time.Duration(p.Multiplier)
	}
	if delay > p.MaximumDelay {
		return p.MaximumDelay, nil
	}
	return delay, nil
}

// BatchLimits bounds one independently signed delivery.
type BatchLimits struct {
	MaximumEvents uint32
	MaximumBytes  uint64
}

// Validate checks an encoded delivery before allocation and again after
// content decoding. MaximumBytes applies independently to both representations.
func (l BatchLimits) Validate(eventCount, transmittedBytes, decodedBytes int) error {
	if l.MaximumEvents < 1 || l.MaximumBytes < 1 || eventCount < 1 || transmittedBytes < 1 || decodedBytes < 1 ||
		uint64(eventCount) > uint64(l.MaximumEvents) || uint64(transmittedBytes) > l.MaximumBytes || uint64(decodedBytes) > l.MaximumBytes {
		return errors.New("webhook batch exceeds negotiated limits")
	}
	return nil
}

// DeliveryState is the durable state-machine vocabulary owned by adapters.
type DeliveryState string

const (
	DeliveryPending      DeliveryState = "pending"
	DeliveryInFlight     DeliveryState = "in-flight"
	DeliverySucceeded    DeliveryState = "succeeded"
	DeliveryDeadLettered DeliveryState = "dead-lettered"
)

// DeliveryRecord contains only durable ownership and opaque payload location.
// Event payload bytes are intentionally absent.
type DeliveryRecord struct {
	Tenant           string
	EndpointID       string
	EndpointRevision string
	DeliveryID       string
	EventIDs         []string
	PayloadReference string
	State            DeliveryState
	Attempt          uint32
	LeaseOwner       string
	FailureCode      string
}

// RecoverDelivery models process-crash recovery and endpoint revocation. It
// never changes the tenant, endpoint revision, delivery ID, event IDs, or
// opaque payload reference.
func RecoverDelivery(record DeliveryRecord, endpoint EndpointRegistration) (DeliveryRecord, error) {
	if err := ValidateEndpointRegistration(endpoint); err != nil {
		return DeliveryRecord{}, err
	}
	if record.Tenant != endpoint.Tenant || record.EndpointID != endpoint.ID || record.EndpointRevision != endpoint.Revision ||
		record.State != DeliveryInFlight || record.Attempt < 1 || !boundedText(record.LeaseOwner, 128) ||
		!boundedText(record.DeliveryID, 128) || len(record.EventIDs) == 0 || !uniqueTexts(record.EventIDs) || !boundedText(record.PayloadReference, 512) {
		return DeliveryRecord{}, ErrDeliveryOwnership
	}
	recovered := record
	recovered.EventIDs = slices.Clone(record.EventIDs)
	recovered.LeaseOwner = ""
	if endpoint.Status == EndpointRevoked {
		recovered.State = DeliveryDeadLettered
		recovered.FailureCode = "ENDPOINT_REVOKED"
		return recovered, nil
	}
	if endpoint.Status != EndpointActive {
		return DeliveryRecord{}, ErrInvalidEndpoint
	}
	if recovered.State == DeliveryInFlight {
		recovered.State = DeliveryPending
	}
	return recovered, nil
}

// DeliveryObservation is the safe default telemetry shape. It deliberately
// cannot carry endpoint URLs, bodies, payload references, digests, signatures,
// keys, or secrets.
type DeliveryObservation struct {
	TenantReference   string    `json:"tenantReference"`
	EndpointReference string    `json:"endpointReference"`
	DeliveryID        string    `json:"deliveryId"`
	EventIDs          []string  `json:"eventIds"`
	EventTypes        []string  `json:"eventTypes"`
	Attempt           uint32    `json:"attempt"`
	Outcome           string    `json:"outcome"`
	FailureCode       string    `json:"failureCode,omitempty"`
	HTTPStatus        int       `json:"httpStatus,omitempty"`
	NextAttempt       time.Time `json:"nextAttempt,omitempty"`
}

func publicAddress(address netip.Addr) bool {
	if !address.IsValid() || !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() ||
		address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsMulticast() || address.IsUnspecified() {
		return false
	}
	return !slices.ContainsFunc(nonPublicPrefixes, func(prefix netip.Prefix) bool { return prefix.Contains(address) })
}

func uniqueTexts(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !boundedText(value, 128) || strings.ContainsAny(value, "\x00\r\n") {
			return false
		}
		if _, ok := seen[value]; ok {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}
