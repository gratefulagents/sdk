package openai

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"strings"
)

const gratefulCABundleEnv = "GRATEFUL_CA_BUNDLE"

func newOpenAIBaseTransport() http.RoundTripper {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return http.DefaultTransport
	}
	transport := base.Clone()
	if roots := openAIClientRootCAs(); roots != nil {
		if transport.TLSClientConfig == nil {
			transport.TLSClientConfig = &tls.Config{}
		} else {
			transport.TLSClientConfig = transport.TLSClientConfig.Clone()
		}
		transport.TLSClientConfig.RootCAs = roots
	}
	return transport
}

func openAIClientRootCAs() *x509.CertPool {
	pool, err := x509.SystemCertPool()
	systemLoaded := err == nil && pool != nil
	if !systemLoaded {
		pool = x509.NewCertPool()
	}
	appended := 0
	for _, path := range openAICABundlePaths() {
		if err := appendCertFile(pool, path); err != nil {
			continue
		}
		appended++
	}
	if !systemLoaded && appended == 0 {
		return nil
	}
	return pool
}

func openAICABundlePaths() []string {
	seen := map[string]bool{}
	var paths []string
	for _, key := range []string{gratefulCABundleEnv, "SSL_CERT_FILE"} {
		path := strings.TrimSpace(os.Getenv(key))
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		paths = append(paths, path)
	}
	return paths
}

func appendCertFile(pool *x509.CertPool, path string) error {
	if pool == nil {
		return fmt.Errorf("nil cert pool")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !pool.AppendCertsFromPEM(data) {
		return fmt.Errorf("no certificates appended from %s", path)
	}
	return nil
}
