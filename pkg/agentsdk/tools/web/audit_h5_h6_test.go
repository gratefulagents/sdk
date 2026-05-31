package web

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"
)

// H5 — decompression bomb / autodecompress past size cap.

func TestSafeHTTPClientDisablesAutoCompression(t *testing.T) {
	client := NewSafeHTTPClient(0)
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", client.Transport)
	}
	if !transport.DisableCompression {
		t.Fatal("DisableCompression must be true so the byte cap survives gzip/deflate/br")
	}
}

func TestFetchToolDoesNotAutoDecompressGzipBomb(t *testing.T) {
	// Construct a tiny gzip payload that decompresses to far more than maxRawBodySize.
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	// 8 MiB of repeated bytes compresses to a few KiB.
	chunk := bytes.Repeat([]byte("A"), 4096)
	for i := 0; i < 2048; i++ {
		if _, err := gw.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	compressed := buf.Bytes()
	if len(compressed) > maxRawBodySize {
		t.Fatalf("compressed payload too big for the test (%d)", len(compressed))
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", itoa(len(compressed)))
		_, _ = w.Write(compressed)
	}))
	defer server.Close()

	input, _ := json.Marshal(fetchInput{URL: server.URL})
	result, err := (&FetchTool{AllowPrivateNetworkURLs: true}).Execute(context.Background(), input, "")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	// If transparent decompression were active, the response body would expand
	// to 8 MiB of "A"s before the size cap was applied. With auto-decompression
	// disabled, we must see at most maxRawBodySize raw bytes.
	if strings.Contains(result.Content, strings.Repeat("A", 1<<20)) {
		t.Fatalf("response was auto-decompressed; size cap bypassed")
	}
}

func itoa(n int) string {
	// avoid pulling strconv into other tests; tiny helper.
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// H6 — IPv6 cloud metadata + private-range coverage.

func TestValidatePublicHTTPURLBlocksIPv6Private(t *testing.T) {
	cases := []string{
		"http://[::1]/",                      // loopback
		"http://[::]/",                       // unspecified
		"http://[::ffff:127.0.0.1]/",         // IPv4-mapped loopback
		"http://[::ffff:7f00:1]/",            // IPv4-mapped loopback (hex)
		"http://[::ffff:10.0.0.1]/",          // IPv4-mapped RFC1918
		"http://[::ffff:169.254.169.254]/",   // IPv4-mapped AWS metadata
		"http://[fd00:ec2::254]/",            // AWS IMDS IPv6
		"http://[fc00::1]/",                  // IPv6 ULA
		"http://[fe80::1]/",                  // IPv6 link-local
		"http://[ff02::1]/",                  // IPv6 multicast
		"http://[2001:db8::1]/",              // documentation
		"http://[64:ff9b::7f00:1]/",          // NAT64 wrapping loopback
	}
	for _, raw := range cases {
		_, err := ValidatePublicHTTPURL(context.Background(), raw)
		if err == nil {
			t.Errorf("%s must be rejected as private/local", raw)
		}
	}
}

func TestValidatePublicHTTPURLAcceptsPublicIPv6(t *testing.T) {
	// 2606:4700:4700::1111 is Cloudflare 1.1.1.1's public IPv6 address.
	_, err := ValidatePublicHTTPURL(context.Background(), "http://[2606:4700:4700::1111]/")
	if err != nil {
		t.Fatalf("public IPv6 must be allowed; got %v", err)
	}
}

// Cloud metadata endpoints outside link-local 169.254/16 must still be blocked
// as defense-in-depth against SSRF targeting Alibaba and Oracle Cloud.
func TestValidatePublicHTTPURLBlocksAlibabaAndOracleMetadata(t *testing.T) {
	cases := []string{
		"http://100.100.100.200/latest/meta-data/", // Alibaba Cloud metadata
		"http://192.0.0.192/opc/v2/instance/",      // Oracle Cloud metadata
		"http://192.0.0.193/",                      // Oracle range /29
		"http://192.0.0.197/",                      // Oracle range /29
	}
	for _, raw := range cases {
		_, err := ValidatePublicHTTPURL(context.Background(), raw)
		if err == nil {
			t.Errorf("%s must be rejected as cloud metadata endpoint", raw)
		}
	}
}

func TestValidatePublicHTTPURLBlocksSSRFFixtureCorpus(t *testing.T) {
	data, err := os.ReadFile("../../../../eval/audit-fixtures/ssrf_urls.txt")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	oldLookup := lookupNetIP
	t.Cleanup(func() { lookupNetIP = oldLookup })
	// Stub DNS so private-named hosts in the corpus resolve to private space
	// rather than relying on the network.
	lookupNetIP = func(_ context.Context, _, host string) ([]netip.Addr, error) {
		switch host {
		case "internal.corp":
			return []netip.Addr{netip.MustParseAddr("10.0.0.1")}, nil
		}
		return nil, errFakeNXDOMAIN
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if _, err := ValidatePublicHTTPURL(context.Background(), line); err == nil {
			t.Errorf("SSRF corpus URL %q must be rejected", line)
		}
	}
}

var errFakeNXDOMAIN = &fakeDNSErr{}

type fakeDNSErr struct{}

func (*fakeDNSErr) Error() string { return "fake nxdomain" }
