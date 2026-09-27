package giftwrap

import (
	"encoding/json"
	"testing"

	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip44"
)

func pair() (string, string) {
	sk := nostr.GeneratePrivateKey()
	pk, _ := nostr.GetPublicKey(sk)
	return sk, pk
}

func TestWrapUnwrap(t *testing.T) {
	aliceSK, alicePK := pair()
	bobSK, bobPK := pair()
	inner, err := NewInner(aliceSK, bobPK, "000102030405060708090a0b0c0d0e0f", "chat", map[string]string{"text": "hi"}, nostr.Now())
	if err != nil {
		t.Fatal(err)
	}
	wrap, err := Wrap(aliceSK, inner, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if wrap.Kind != KindWrap || wrap.PubKey == alicePK || wrap.Tags.Find("p")[1] != bobPK || wrap.CreatedAt != inner.CreatedAt {
		t.Fatalf("wrap %+v", wrap)
	}
	got, err := Unwrap(bobSK, wrap)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != inner.ID || got.PubKey != alicePK || Type(got) != "chat" || OrderID(got) != "000102030405060708090a0b0c0d0e0f" {
		t.Fatalf("inner %+v", got)
	}
	carolSK, _ := pair()
	if _, err := Unwrap(carolSK, wrap); err == nil {
		t.Fatal("unwrapped by a third party")
	}
}

// A seal by one key around an inner event by another must be refused (§4.1).
func TestSealAuthorMustMatch(t *testing.T) {
	aliceSK, _ := pair()
	mallorySK, _ := pair()
	bobSK, bobPK := pair()
	inner, _ := NewInner(aliceSK, bobPK, "", "chat", map[string]string{}, nostr.Now())
	// mallory reseals alice's inner event
	innerJSON, _ := json.Marshal(inner)
	ck, _ := nip44.GenerateConversationKey(bobPK, mallorySK)
	sealContent, _ := nip44.Encrypt(string(innerJSON), ck)
	seal := &nostr.Event{Kind: KindSeal, CreatedAt: inner.CreatedAt, Tags: nostr.Tags{}, Content: sealContent}
	_ = seal.Sign(mallorySK)
	sealJSON, _ := json.Marshal(seal)
	eph := nostr.GeneratePrivateKey()
	ck2, _ := nip44.GenerateConversationKey(bobPK, eph)
	wc, _ := nip44.Encrypt(string(sealJSON), ck2)
	wrap := &nostr.Event{Kind: KindWrap, CreatedAt: inner.CreatedAt, Tags: nostr.Tags{{"p", bobPK}}, Content: wc}
	_ = wrap.Sign(eph)
	if _, err := Unwrap(bobSK, wrap); err == nil {
		t.Fatal("resealed inner event of another author accepted")
	}

	// a tampered inner event fails its signature
	tampered := *inner
	tampered.Content = `{"text":"changed"}`
	if err := VerifyInner(&tampered); err == nil {
		t.Fatal("tampered inner accepted")
	}
}
