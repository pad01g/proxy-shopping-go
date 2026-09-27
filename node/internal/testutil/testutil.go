// Package testutil holds helpers shared by tests: in-process relays and fixtures.
package testutil

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/pad01g/proxy-shopping-go/node/internal/relay"
)

// StartRelay runs a psrelay in-process and returns its ws:// URL.
func StartRelay(t testing.TB) string {
	t.Helper()
	srv, err := relay.New(relay.Options{DataDir: t.TempDir(), Log: Logger(t)})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv)
	t.Cleanup(func() {
		hs.CloseClientConnections()
		hs.Close()
		srv.Close()
	})
	return "ws" + strings.TrimPrefix(hs.URL, "http")
}

// Logger logs to stderr when PS_TEST_LOG is set and discards otherwise.
func Logger(t testing.TB) *slog.Logger {
	var w io.Writer = io.Discard
	if os.Getenv("PS_TEST_LOG") != "" {
		w = os.Stderr
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug})).With("test", t.Name())
}
