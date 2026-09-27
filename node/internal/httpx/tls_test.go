package httpx

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExtraCA(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) }))
	defer srv.Close()
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	_ = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600)

	tc, err := TLSConfig(ca)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Client(tc, 5*time.Second).Get(srv.URL)
	if err != nil {
		t.Fatalf("extra CA not trusted: %v", err)
	}
	res.Body.Close()

	plain, _ := TLSConfig("")
	if _, err := Client(plain, 5*time.Second).Get(srv.URL); err == nil {
		t.Fatal("self-signed server trusted without the extra CA")
	}
	bad := filepath.Join(dir, "bad.pem")
	_ = os.WriteFile(bad, []byte("nope"), 0o600)
	if _, err := TLSConfig(bad); err == nil {
		t.Fatal("file without certificates accepted")
	}
	if err := WaitForFile(filepath.Join(dir, "never"), 10*time.Millisecond); err == nil {
		t.Fatal("missing file reported present")
	}
}
