package web

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

const (
	urlValidationTimeout = 5 * time.Second
)

type URLSecurityOptions struct {
	AllowPrivateNetworkURLs bool
}

var lookupNetIP = func(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, network, host)
}

// ValidatePublicHTTPURL validates an agent-supplied HTTP(S) URL and rejects
// private/local destinations.
func ValidatePublicHTTPURL(ctx context.Context, rawURL string) (*url.URL, error) {
	return ValidateHTTPURL(ctx, rawURL, URLSecurityOptions{})
}

// ValidateHTTPURL validates an agent-supplied HTTP(S) URL. Private/local
// destinations are rejected unless opts explicitly allows them.
func ValidateHTTPURL(ctx context.Context, rawURL string, opts URLSecurityOptions) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("url must start with http:// or https://")
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("url must not contain embedded credentials")
	}
	host := strings.TrimSpace(parsed.Hostname())
	if host == "" {
		return nil, fmt.Errorf("url host is required")
	}
	if opts.AllowPrivateNetworkURLs {
		return parsed, nil
	}
	if isBlockedHostname(host) {
		return nil, fmt.Errorf("url host %q is private or local", host)
	}
	addrs, err := resolveHostForURL(ctx, host)
	if err != nil {
		return nil, err
	}
	for _, addr := range addrs {
		if !isPublicRoutableIP(addr) {
			return nil, fmt.Errorf("url host %q resolves to private or local address %s", host, addr)
		}
	}
	return parsed, nil
}

func newSafeHTTPClient(timeout time.Duration) *http.Client {
	return newSafeHTTPClientWithOptions(timeout, URLSecurityOptions{})
}

func newSafeHTTPClientWithOptions(timeout time.Duration, opts URLSecurityOptions) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = safeDialContextWithOptions(opts)
	// Disable automatic Accept-Encoding/decompression so byte caps in callers
	// (io.LimitReader on resp.Body) apply to the wire bytes instead of the
	// post-decompression stream — defeats gzip/deflate/br decompression bombs.
	transport.DisableCompression = true
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			_, err := ValidateHTTPURL(req.Context(), req.URL.String(), opts)
			return err
		},
	}
}

// NewSafeHTTPClient returns an HTTP client that revalidates every dial target
// and refuses private/local destinations.
func NewSafeHTTPClient(timeout time.Duration) *http.Client {
	return newSafeHTTPClient(timeout)
}

// NewSafeHTTPClientWithOptions returns an HTTP client that revalidates every
// dial target using explicit URL security options.
func NewSafeHTTPClientWithOptions(timeout time.Duration, opts URLSecurityOptions) *http.Client {
	return newSafeHTTPClientWithOptions(timeout, opts)
}

func safeDialContextWithOptions(opts URLSecurityOptions) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		return safeDialContext(ctx, network, address, opts)
	}
}

func safeDialContext(ctx context.Context, network, address string, opts URLSecurityOptions) (net.Conn, error) {
	if opts.AllowPrivateNetworkURLs {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addrs, err := resolveHostForURL(ctx, host)
	if err != nil {
		return nil, err
	}
	for _, addr := range addrs {
		if !isPublicRoutableIP(addr) {
			return nil, fmt.Errorf("refusing to dial private or local address %s for host %q", addr, host)
		}
	}
	var lastErr error
	dialer := &net.Dialer{}
	for _, addr := range addrs {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(addr.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	// Defensive fallthrough: resolveHostForURL guarantees len(addrs) > 0, so
	// the dialer loop above always runs at least once and either returns a
	// connection or sets lastErr. This branch is therefore unreachable in
	// practice; it exists so future refactors that change the resolver or
	// loop semantics still produce a typed error instead of `(nil, nil)`.
	return nil, fmt.Errorf("host %q resolved to no dialable addresses", host)
}

func resolveHostForURL(ctx context.Context, host string) ([]netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{addr}, nil
	}
	lookupCtx, cancel := context.WithTimeout(ctx, urlValidationTimeout)
	defer cancel()
	addrs, err := lookupNetIP(lookupCtx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolve url host %q: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("resolve url host %q: no addresses", host)
	}
	return addrs, nil
}

func isBlockedHostname(host string) bool {
	lower := strings.ToLower(strings.TrimSuffix(host, "."))
	return lower == "localhost" ||
		strings.HasSuffix(lower, ".localhost") ||
		strings.HasSuffix(lower, ".local") ||
		lower == "metadata.google.internal"
}

func isPublicRoutableIP(addr netip.Addr) bool {
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	if !addr.IsValid() || !addr.IsGlobalUnicast() ||
		addr.IsLoopback() || addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		addr.IsMulticast() || addr.IsUnspecified() {
		return false
	}
	for _, prefix := range blockedIPPrefixes() {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

func blockedIPPrefixes() []netip.Prefix {
	return []netip.Prefix{
		// IPv4
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("169.254.0.0/16"),
		netip.MustParsePrefix("198.18.0.0/15"),
		// Cloud metadata services (defense-in-depth; some are outside
		// 169.254.0.0/16 / link-local ranges).
		netip.MustParsePrefix("100.100.100.200/32"), // Alibaba Cloud metadata
		netip.MustParsePrefix("192.0.0.192/29"),     // Oracle Cloud metadata
		// IPv6 — defense-in-depth alongside netip.Addr.IsLoopback / IsPrivate /
		// IsLinkLocalUnicast / IsMulticast / IsUnspecified checks above.
		netip.MustParsePrefix("::/128"),         // unspecified
		netip.MustParsePrefix("::1/128"),        // loopback
		netip.MustParsePrefix("::ffff:0:0/96"),  // IPv4-mapped (defense-in-depth; we Unmap above)
		netip.MustParsePrefix("64:ff9b::/96"),   // NAT64 (RFC 6052) — can wrap private IPv4
		netip.MustParsePrefix("64:ff9b:1::/48"), // NAT64 local-use (RFC 8215)
		netip.MustParsePrefix("100::/64"),       // discard prefix (RFC 6666)
		netip.MustParsePrefix("2001:db8::/32"),  // documentation (RFC 3849)
		netip.MustParsePrefix("fc00::/7"),       // unique local addresses (incl. AWS IMDSv6 fd00:ec2::254)
		netip.MustParsePrefix("fe80::/10"),      // link-local
		netip.MustParsePrefix("fec0::/10"),      // site-local (deprecated)
		netip.MustParsePrefix("ff00::/8"),       // multicast
	}
}
