package keys

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const abandon = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"

func TestNIP06Vector(t *testing.T) {
	// test vector of NIP-06
	s, err := FromMnemonic("leader monkey parrot ring guide accident before fence cannon height naive bean")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.NostrSecretHex(); got != "7f7ff03d123792d6ac594bfa67bf6d0c0ab55b6b1fdb6249303fe861f1ccba9a" {
		t.Fatalf("secret %s", got)
	}
	if got := s.NostrPubHex(); got != "17162c921dc4d2518f9a101db33695df1afb56ab82f5ff3e5da6eec3ca5cd917" {
		t.Fatalf("pubkey %s", got)
	}
}

func TestBIP84Vector(t *testing.T) {
	// BIP84 test vector (testnet coin type 1 differs from the BIP's mainnet vector, so check the EVM one instead)
	s, err := FromMnemonic(abandon)
	if err != nil {
		t.Fatal(err)
	}
	// well known: first Ethereum account of the "abandon … about" mnemonic
	if got := s.EVMAddress().Hex(); got != "0x9858EfFD232B4033E47d90003D41EC34EcaEda94" {
		t.Fatalf("evm %s", got)
	}
	if !strings.HasPrefix(s.WalletAddress(), "tb1q") {
		t.Fatalf("wallet %s", s.WalletAddress())
	}
	xpub, err := s.EscrowXpub()
	if err != nil || !strings.HasPrefix(xpub, "tpub") {
		t.Fatalf("xpub %q %v", xpub, err)
	}
}

func TestLabKeysMatchPublicJSON(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "lab", "keys")
	data, err := os.ReadFile(filepath.Join(dir, "public.json"))
	if err != nil {
		t.Skipf("no lab/keys/public.json: %v", err)
	}
	var want map[string]struct {
		NostrPubkey string `json:"nostr_pubkey"`
		PeerID      string `json:"libp2p_peer_id"`
	}
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	for name, w := range want {
		s, err := LoadMnemonicFile(filepath.Join(dir, name+".mnemonic"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if s.NostrPubHex() != w.NostrPubkey {
			t.Errorf("%s nostr %s, want %s", name, s.NostrPubHex(), w.NostrPubkey)
		}
		id, err := s.PeerID()
		if err != nil || id.String() != w.PeerID {
			t.Errorf("%s peer id %s, want %s (%v)", name, id, w.PeerID, err)
		}
	}
}

func TestOrderIndexAndEscrowChild(t *testing.T) {
	idx, err := OrderIndex("000102030405060708090a0b0c0d0e0f")
	if err != nil {
		t.Fatal(err)
	}
	if idx&0x80000000 != 0 {
		t.Fatal("idx must be below 2^31")
	}
	if _, err := OrderIndex("0001"); err == nil {
		t.Fatal("short order id accepted")
	}
	if _, err := OrderIndex("000102030405060708090A0B0C0D0E0F"); err == nil {
		t.Fatal("upper case order id accepted")
	}

	s, err := FromMnemonic(abandon)
	if err != nil {
		t.Fatal(err)
	}
	xpub, _ := s.EscrowXpub()
	oid := "000102030405060708090a0b0c0d0e0f"
	pub, err := EscrowChildPubKey(xpub, oid)
	if err != nil {
		t.Fatal(err)
	}
	priv, err := s.EscrowOrderKey(oid)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(pub.SerializeCompressed()) != hex.EncodeToString(priv.PubKey().SerializeCompressed()) {
		t.Fatal("public derivation from the xpub differs from the private derivation")
	}
	a, _ := s.OrderKey(oid)
	b, _ := s.OrderKey("ffffffffffffffffffffffffffffffff")
	if a.Key.Equals(&b.Key) {
		t.Fatal("different orders gave the same key")
	}
}
