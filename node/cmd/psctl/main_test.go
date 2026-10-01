package main

import (
	"bytes"
	"encoding/json"
	"os"
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

// psctl list --bundle-out writes the §2.6 bundle: the list, then the newest valid profiles and inbox relays of its
// shoppers and escrows, unchanged.
func TestListBundleOut(t *testing.T) {
	dir := t.TempDir()
	labKeys := filepath.Join("..", "..", "..", "lab", "keys")
	sh, err := keys.LoadMnemonicFile(filepath.Join(labKeys, "shopper-1.mnemonic"))
	if err != nil {
		t.Fatal(err)
	}
	es, err := keys.LoadMnemonicFile(filepath.Join(labKeys, "escrow-1.mnemonic"))
	if err != nil {
		t.Fatal(err)
	}
	must := func(ev *nostr.Event, err error) *nostr.Event {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return ev
	}
	oldProfile := must(trust.NewProfile(sh.NostrSecretHex(), trust.KindShopperProfile, "ps-lab", 1, map[string]string{"name": "old"}))
	newProfile := must(trust.NewProfile(sh.NostrSecretHex(), trust.KindShopperProfile, "ps-lab", 2, map[string]string{"name": "new"}))
	forged := *must(trust.NewProfile(sh.NostrSecretHex(), trust.KindShopperProfile, "ps-lab", 3, map[string]string{"name": "x"}))
	forged.Content = `{"name":"forged"}`
	otherNet := must(trust.NewProfile(sh.NostrSecretHex(), trust.KindShopperProfile, "ps-main", 9, map[string]string{"name": "main"}))
	escrowProfile := must(trust.NewProfile(es.NostrSecretHex(), trust.KindEscrowProfile, "ps-lab", 5, map[string]string{"name": "e"}))
	inbox := must(trust.NewInboxRelays(sh.NostrSecretHex(), []string{"wss://relay-1.test"}, 100))
	stranger := must(trust.NewProfile(nostr.GeneratePrivateKey(), trust.KindShopperProfile, "ps-lab", 5, map[string]string{"name": "s"}))
	profiles, _ := json.Marshal(trust.Bundle{Events: []*nostr.Event{oldProfile, inbox, &forged, otherNet, newProfile, escrowProfile, stranger}})
	writeFile(t, filepath.Join(dir, "profiles.json"), profiles)
	list := map[string]any{
		"name": "op", "regions": []string{"JP"}, "relays": []any{}, "p2p_relays": []string{"/dns4/relay.example/tcp/443/tls/ws/p2p/16Uiu2HAmFngaVFK4D5fix5qN2c6guh3LNyBkmuB42vTFW9aaeDZh"},
		"entries": []map[string]any{{"region": "JP", "shopper": sh.NostrPubHex(), "escrow": es.NostrPubHex(), "shops": []string{"*"}, "payments": []string{"btc-signet"}, "tags": []string{}, "escrow_sla_days": 14}},
	}
	data, _ := json.Marshal(list)
	writeFile(t, filepath.Join(dir, "list.json"), data)
	out := filepath.Join(dir, "bundle.json")
	if err := cmdList([]string{"--mnemonic-file", filepath.Join(labKeys, "operator-1.mnemonic"), "--file", filepath.Join(dir, "list.json"),
		"--version", "7", "--bundle-out", out, "--profiles-file", filepath.Join(dir, "profiles.json")}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	evs, err := trust.ParseBundle(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 4 {
		t.Fatalf("bundle has %d events, want list + 30502 + 30503 + 10050", len(evs))
	}
	if evs[0].Kind != trust.KindList || trust.Version(evs[0]) != 7 {
		t.Fatalf("first event %d v%d", evs[0].Kind, trust.Version(evs[0]))
	}
	l, err := trust.ParseList(evs[0])
	if err != nil || len(l.P2PRelays) != 1 {
		t.Fatalf("p2p_relays not kept in the list: %v %+v", err, l)
	}
	ids := map[string]bool{}
	for _, ev := range evs[1:] {
		ids[ev.ID] = true
	}
	if !ids[newProfile.ID] || !ids[escrowProfile.ID] || !ids[inbox.ID] {
		t.Fatalf("bundle lacks the newest profiles: %+v", evs)
	}
	// unchanged: still verifies
	for _, ev := range evs {
		if err := trust.Validate(ev); err != nil {
			t.Fatalf("event of the bundle does not verify: %v", err)
		}
	}
}

// psctl delegate --list-url adds the tags (§2.2) and refuses what is not https.
func TestDelegateListURL(t *testing.T) {
	sk := nostr.GeneratePrivateKey()
	op, _ := nostr.GetPublicKey(nostr.GeneratePrivateKey())
	ev, err := trust.NewDelegationWithURLs(sk, op, "ps-lab", 3, false, "", []string{"https://op.example/bundle.json", "https://mirror.example/b.json"})
	if err != nil {
		t.Fatal(err)
	}
	if got := trust.ListURLs(ev); len(got) != 2 || got[0] != "https://op.example/bundle.json" {
		t.Fatalf("list_url tags %v", got)
	}
	if _, err := trust.NewDelegationWithURLs(sk, op, "ps-lab", 3, false, "", []string{"http://op.example/b.json"}); err == nil {
		t.Fatal("http list_url accepted")
	}
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
