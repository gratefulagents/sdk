package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestFetchToolRejectsLocalhostByDefault(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("local"))
	}))
	defer server.Close()

	input, _ := json.Marshal(fetchInput{URL: server.URL})
	result, err := (&FetchTool{}).Execute(context.Background(), input, "")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content, "private or local") {
		t.Fatalf("result = %#v, want private/local rejection", result)
	}
}

func TestFetchToolAllowsLocalhostWhenExplicitlyConfigured(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("local ok"))
	}))
	defer server.Close()

	input, _ := json.Marshal(fetchInput{URL: server.URL})
	result, err := (&FetchTool{AllowPrivateNetworkURLs: true}).Execute(context.Background(), input, "")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.IsError || !strings.Contains(result.Content, "local ok") {
		t.Fatalf("result = %#v, want fetched local content", result)
	}
}

func TestFetchToolRejectsDNSRebindAtDialTime(t *testing.T) {
	oldLookup := lookupNetIP
	t.Cleanup(func() { lookupNetIP = oldLookup })

	var lookups int
	lookupNetIP = func(ctx context.Context, network, host string) ([]netip.Addr, error) {
		if host != "rebind.example" {
			return oldLookup(ctx, network, host)
		}
		lookups++
		if lookups == 1 {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}

	input, _ := json.Marshal(fetchInput{URL: "http://rebind.example/"})
	result, err := (&FetchTool{}).Execute(context.Background(), input, "")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content, "refusing to dial private or local address") {
		t.Fatalf("result = %#v, want safe dial DNS-rebind rejection", result)
	}
	if lookups < 2 {
		t.Fatalf("lookups = %d, want validation lookup plus dial-time revalidation", lookups)
	}
}

func TestFetchToolRejectsPrivateRedirect(t *testing.T) {
	oldLookup := lookupNetIP
	t.Cleanup(func() { lookupNetIP = oldLookup })

	lookupNetIP = func(ctx context.Context, network, host string) ([]netip.Addr, error) {
		switch host {
		case "redirect.example":
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		default:
			return oldLookup(ctx, network, host)
		}
	}

	client := NewSafeHTTPClient(0)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://redirect.example/", nil)
	if err != nil {
		t.Fatal(err)
	}
	redirectReq, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://127.0.0.1/", nil)
	if err != nil {
		t.Fatal(err)
	}
	err = client.CheckRedirect(redirectReq, []*http.Request{req})
	if err == nil || !strings.Contains(err.Error(), "private or local") {
		t.Fatalf("CheckRedirect() error = %v, want private redirect rejection", err)
	}
}

func TestSafeHTTPClientRejectsPrivateDial(t *testing.T) {
	client := NewSafeHTTPClient(0)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://127.0.0.1:1/", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(req)
	if err == nil || !strings.Contains(err.Error(), "refusing to dial private or local address") {
		t.Fatalf("client.Do() error = %v, want private dial rejection", err)
	}
}

func TestValidatePublicHTTPURLUsesInjectedLookup(t *testing.T) {
	oldLookup := lookupNetIP
	t.Cleanup(func() { lookupNetIP = oldLookup })

	lookupNetIP = func(context.Context, string, string) ([]netip.Addr, error) {
		return nil, fmt.Errorf("lookup sentinel")
	}
	_, err := ValidatePublicHTTPURL(context.Background(), "http://lookup.example/")
	if err == nil || !strings.Contains(err.Error(), "lookup sentinel") {
		t.Fatalf("ValidatePublicHTTPURL() error = %v, want injected lookup error", err)
	}
}
