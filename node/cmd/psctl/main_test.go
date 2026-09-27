package main

import (
	"path/filepath"
	"testing"

	"github.com/nbd-wtf/go-nostr/nip19"

	"github.com/pad01g/proxy-shopping-go/node/internal/keys"
)

func TestKeysOutputAndPubkey(t *testing.T) {
	s, err := keys.LoadMnemonicFile(filepath.Join("..", "..", "..", "lab", "keys", "relay-p2p.mnemonic"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := keysOutput(s)
	if err != nil {
		t.Fatal(err)
	}
	if out.Libp2pPeerID != "16Uiu2HAmFngaVFK4D5fix5qN2c6guh3LNyBkmuB42vTFW9aaeDZh" || out.NostrPubkey != "f73a47e88d82feb7b745d2a41eddddfd91447b0ea7528749c4e172e3fef76043" {
		t.Fatalf("%+v", out)
	}
	npub, _ := nip19.EncodePublicKey(out.NostrPubkey)
	for _, in := range []string{out.NostrPubkey, npub} {
		if pk, err := pubkey(in); err != nil || pk != out.NostrPubkey {
			t.Fatalf("pubkey(%s) = %s, %v", in, pk, err)
		}
	}
	if _, err := pubkey("zz"); err == nil {
		t.Fatal("garbage accepted")
	}
}
