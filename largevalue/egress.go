package largevalue

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

type DNSResolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

type EgressPolicy struct {
	Resolver         DNSResolver
	AllowedOrigins   []string
	AllowedSchemes   []string
	AllowedPorts     []uint16
	MaximumRedirects int
}

// ValidateHop resolves and validates every request or redirect hop. Callers
// must call it immediately before dialing and pin the returned addresses for
// that dial rather than resolving the hostname again in a default transport.
func (p EgressPolicy) ValidateHop(ctx context.Context, rawURL string) ([]netip.Addr, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, ErrEgressDenied
	}
	scheme := strings.ToLower(parsed.Scheme)
	allowedSchemes := p.AllowedSchemes
	if len(allowedSchemes) == 0 {
		allowedSchemes = []string{"https"}
	}
	if !slices.Contains(allowedSchemes, scheme) {
		return nil, ErrEgressDenied
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") || host == "metadata.google.internal" {
		return nil, ErrEgressDenied
	}
	port, err := egressPort(parsed, scheme)
	if err != nil || (len(p.AllowedPorts) > 0 && !slices.Contains(p.AllowedPorts, port)) {
		return nil, ErrEgressDenied
	}
	origin := scheme + "://" + net.JoinHostPort(host, strconv.Itoa(int(port)))
	if len(p.AllowedOrigins) > 0 && !slices.Contains(p.AllowedOrigins, origin) {
		return nil, ErrEgressDenied
	}
	addresses, err := resolveAddresses(ctx, p.Resolver, host)
	if err != nil || len(addresses) == 0 {
		return nil, ErrEgressDenied
	}
	for _, address := range addresses {
		if unsafeAddress(address.Unmap()) {
			return nil, ErrEgressDenied
		}
	}
	return addresses, nil
}

// ValidateRedirectChain validates every hop and re-resolves every hostname,
// which makes DNS changes and redirects subject to the same policy.
func (p EgressPolicy) ValidateRedirectChain(ctx context.Context, chain []string) error {
	maximum := p.MaximumRedirects
	if maximum == 0 {
		maximum = DefaultMaximumRedirects
	}
	if len(chain) == 0 || len(chain)-1 > maximum {
		return ErrEgressDenied
	}
	for _, rawURL := range chain {
		if _, err := p.ValidateHop(ctx, rawURL); err != nil {
			return err
		}
	}
	return nil
}

func resolveAddresses(ctx context.Context, resolver DNSResolver, host string) ([]netip.Addr, error) {
	if address, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{address}, nil
	}
	if resolver == nil {
		return nil, errorsNoResolver
	}
	return resolver.LookupNetIP(ctx, "ip", host)
}

var errorsNoResolver = fmt.Errorf("%w: DNS resolver required", ErrEgressDenied)

func egressPort(parsed *url.URL, scheme string) (uint16, error) {
	if parsed.Port() == "" {
		if scheme == "https" {
			return 443, nil
		}
		if scheme == "http" {
			return 80, nil
		}
		return 0, ErrEgressDenied
	}
	value, err := strconv.ParseUint(parsed.Port(), 10, 16)
	return uint16(value), err
}

func unsafeAddress(address netip.Addr) bool {
	if !address.IsValid() || address.IsUnspecified() || address.IsLoopback() || address.IsPrivate() ||
		address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsMulticast() {
		return true
	}
	if address.Is4() {
		bytes := address.As4()
		return bytes[0] == 0 || bytes[0] == 127 || (bytes[0] == 169 && bytes[1] == 254) ||
			(bytes[0] == 100 && bytes[1]&0xc0 == 0x40) || bytes[0] >= 224
	}
	return false
}
