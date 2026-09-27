package proto

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

func TestChunksRoundTrip(t *testing.T) {
	big := make([]byte, 3*AttachmentChunk+123)
	_, _ = rand.Read(big)
	sum := sha256.Sum256(big)
	small := []byte(`{"receipt":"ok"}`)
	items := []Evidence{
		{Kind: "screenshot", SHA256: hex.EncodeToString(sum[:]), MIME: "image/png", DataB64: base64.StdEncoding.EncodeToString(big)},
		{Kind: "json", SHA256: "x", MIME: "application/json", DataB64: base64.StdEncoding.EncodeToString(small)},
	}
	inline := InlineOnly(items)
	if inline[0].DataB64 != "" || inline[1].DataB64 == "" {
		t.Fatalf("inline: large must be stripped, small kept: %+v", inline)
	}
	if items[0].DataB64 == "" {
		t.Fatal("InlineOnly must not modify its input")
	}
	chunks, err := Chunks(items)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 4 {
		t.Fatalf("chunks: %d", len(chunks))
	}
	parts := map[int]string{}
	for _, c := range chunks[:3] {
		parts[c.Index] = c.DataB64
	}
	if _, ok, _ := Assemble(parts, 4, chunks[0].SHA256); ok {
		t.Fatal("assembled with a missing chunk")
	}
	parts[3] = chunks[3].DataB64
	data, ok, err := Assemble(parts, 4, chunks[0].SHA256)
	if err != nil || !ok || !bytes.Equal(data, big) {
		t.Fatalf("assemble: ok=%v err=%v", ok, err)
	}
	parts[0] = base64.StdEncoding.EncodeToString([]byte("tampered"))
	if _, _, err := Assemble(parts, 4, chunks[0].SHA256); err == nil {
		t.Fatal("tampered chunk accepted")
	}
}
