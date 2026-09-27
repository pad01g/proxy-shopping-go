package delivery

import (
	"testing"

	"github.com/nbd-wtf/go-nostr"
)

const oid = "000102030405060708090a0b0c0d0e0f"

func TestSealOpen(t *testing.T) {
	k, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	addr := Address{Name: "A", PostalCode: "1", Address: "x", Phone: "2"}
	ct, err := Seal(k, nil, oid, addr)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Open(k, ct, oid)
	if err != nil || got != addr {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := Open(k, ct, "ffffffffffffffffffffffffffffffff"); err == nil {
		t.Fatal("wrong order id (aad) accepted")
	}
	other, _ := NewKey()
	if _, err := Open(other, ct, oid); err == nil {
		t.Fatal("wrong key accepted")
	}
	if _, err := Seal(k, []byte{1}, oid, addr); err == nil {
		t.Fatal("short nonce accepted")
	}
}

func TestWrapKey(t *testing.T) {
	user, shopper := nostr.GeneratePrivateKey(), nostr.GeneratePrivateKey()
	userPub, _ := nostr.GetPublicKey(user)
	shopperPub, _ := nostr.GetPublicKey(shopper)
	k, _ := NewKey()
	w, err := WrapKey(user, shopperPub, k)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnwrapKey(shopper, userPub, w)
	if err != nil || got != k {
		t.Fatalf("unwrap: %v", err)
	}
	stranger := nostr.GeneratePrivateKey()
	if _, err := UnwrapKey(stranger, userPub, w); err == nil {
		t.Fatal("stranger unwrapped the key")
	}
}
