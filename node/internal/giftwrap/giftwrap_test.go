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

// The o tag is required on every message but an ack; NewInner gives messages without an order a random one
// (§4.10, review 2 item 9).
func TestOrderTagRequired(t *testing.T) {
	aliceSK, _ := pair()
	_, bobPK := pair()
	inner, err := NewInner(aliceSK, bobPK, "", "report", map[string]string{}, nostr.Now())
	if err != nil || len(OrderID(inner)) != 32 || VerifyInner(inner) != nil {
		t.Fatalf("order-less message: o %q, err %v", OrderID(inner), err)
	}
	ack, _ := NewInner(aliceSK, bobPK, "", TypeAck, map[string][]string{"ids": {}}, nostr.Now())
	if OrderID(ack) != "" || VerifyInner(ack) != nil {
		t.Fatal("an ack needs no o tag")
	}
	bare := &nostr.Event{Kind: KindInner, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"p", bobPK}, {"t", "chat"}}, Content: "{}"}
	_ = bare.Sign(aliceSK)
	if VerifyInner(bare) == nil {
		t.Fatal("a chat without o accepted")
	}
}
