package proto

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// NIP-44 encrypts at most 64 KiB, and the wrap carries the seal which carries the inner as base64,
// so an inner event must stay well below that (spec §4.9).
const (
	MaxInnerBytes     = 30_000
	InlineEvidenceMax = 8 << 10  // evidence data up to this size travels inside messages
	AttachmentChunk   = 12 << 10 // raw bytes per attachment message (the wrap grows to about 2.3x)
)

// TypeAttachment carries one chunk of a large piece of evidence (spec §4.9).
const TypeAttachment = "attachment"

type Attachment struct {
	SHA256  string `json:"sha256"`
	MIME    string `json:"mime"`
	Index   int    `json:"index"`
	Total   int    `json:"total"`
	DataB64 string `json:"data_b64"`
}

// InlineOnly returns the evidence with data removed from items too large to travel inline.
// The hash stays, so the item can be matched to its attachment later.
func InlineOnly(items []Evidence) []Evidence {
	out := make([]Evidence, len(items))
	for i, ev := range items {
		out[i] = ev
		if base64.StdEncoding.DecodedLen(len(ev.DataB64)) > InlineEvidenceMax {
			out[i].DataB64 = ""
		}
	}
	return out
}

// Chunks splits the data of large evidence items into attachment messages.
func Chunks(items []Evidence) ([]Attachment, error) {
	var out []Attachment
	for _, ev := range items {
		if ev.DataB64 == "" || base64.StdEncoding.DecodedLen(len(ev.DataB64)) <= InlineEvidenceMax {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(ev.DataB64)
		if err != nil {
			return nil, fmt.Errorf("evidence %s: %w", ev.SHA256, err)
		}
		total := (len(data) + AttachmentChunk - 1) / AttachmentChunk
		for i := 0; i < total; i++ {
			end := min((i+1)*AttachmentChunk, len(data))
			out = append(out, Attachment{SHA256: ev.SHA256, MIME: ev.MIME, Index: i, Total: total,
				DataB64: base64.StdEncoding.EncodeToString(data[i*AttachmentChunk : end])})
		}
	}
	return out, nil
}

// Assemble joins the chunks of one attachment and checks the hash. ok is false while chunks are missing.
func Assemble(chunks map[int]string, total int, sum string) (data []byte, ok bool, err error) {
	if len(chunks) < total {
		return nil, false, nil
	}
	for i := 0; i < total; i++ {
		part, err := base64.StdEncoding.DecodeString(chunks[i])
		if err != nil {
			return nil, false, fmt.Errorf("chunk %d: %w", i, err)
		}
		data = append(data, part...)
	}
	h := sha256.Sum256(data)
	if hex.EncodeToString(h[:]) != sum {
		return nil, false, fmt.Errorf("attachment hash mismatch: want %s", sum)
	}
	return data, true, nil
}
