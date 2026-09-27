package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nbd-wtf/go-nostr"

	"github.com/nbd-wtf/go-nostr/nip19"

	"github.com/pad01g/proxy-shopping-go/node/internal/keys"
	"github.com/pad01g/proxy-shopping-go/node/internal/trust"
)

// The version of a new event is above the published one, and an explicit version that is not is warned about
// (the relays and nodes would keep the published one, §2.1).
func TestVersionAboveThePublished(t *testing.T) {
	var warn bytes.Buffer
	if v := pickVersion(0, 0, 1000, &warn); v != 1000 {
		t.Fatalf("no published version: %d", v)
	}
	if v := pickVersion(0, 5000, 1000, &warn); v != 5001 {
		t.Fatalf("published version ahead of the clock: %d", v)
	}
	if v := pickVersion(7, 0, 1000, &warn); v != 7 || warn.Len() != 0 {
		t.Fatalf("explicit version: %d %q", v, warn.String())
	}
	if v := pickVersion(3, 5000, 1000, &warn); v != 3 || !strings.Contains(warn.String(), "not above the published version 5000") {
		t.Fatalf("stale explicit version: %d %q", v, warn.String())
	}
	sk := nostr.GeneratePrivateKey()
	pk, _ := nostr.GetPublicKey(sk)
	other := nostr.GeneratePrivateKey()
	d1, _ := trust.NewDelegation(sk, pk, "ps-lab", 42, false, "")
	d2, _ := trust.NewDelegation(sk, pk, "ps-lab", 99, false, "")
	forged := *d2
	forged.Tags = nostr.Tags{{"d", pk}, {"v", "999999"}, {"network", "ps-lab"}}
	stranger, _ := trust.NewDelegation(other, pk, "ps-lab", 5000, false, "")
	if v := maxVersion([]*nostr.Event{d1, d2, &forged, stranger}, trust.KindDelegation, pk, pk); v != 99 {
		t.Fatalf("known version %d", v)
	}
}

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
