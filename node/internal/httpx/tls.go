// Package httpx builds HTTP clients that also trust the lab CA (tls.extra_ca).
package httpx

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"time"
)

// TLSConfig returns a TLS configuration trusting the system roots plus the PEM file extraCA (if set).
func TLSConfig(extraCA string) (*tls.Config, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if extraCA != "" {
		pem, err := os.ReadFile(extraCA)
		if err != nil {
			return nil, fmt.Errorf("read extra CA: %w", err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificate in %s", extraCA)
		}
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}

// Client returns an HTTP client with the TLS configuration and a timeout.
func Client(tc *tls.Config, timeout time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tc
	return &http.Client{Transport: tr, Timeout: timeout}
}

// WaitForFile waits until path exists (the lab CA appears after Caddy started).
func WaitForFile(path string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		_, err := os.Stat(path)
		if err == nil || time.Now().After(deadline) {
			return err
		}
		time.Sleep(time.Second)
	}
}
