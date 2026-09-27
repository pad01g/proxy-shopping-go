// Package giftwrap implements the three layers of spec §4.1: a signed inner event (kind 5400), a seal (kind 13)
// and a gift wrap (kind 1059) signed by a one-time key.
package giftwrap

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip44"
)

// Event kinds of the message layers.
const (
	KindInner = 5400
	KindSeal  = 13
	KindWrap  = 1059
)

// NewInner builds and signs the inner event. body is marshaled to JSON as the content.
func NewInner(senderSecret, recipient, orderID, typ string, body any, createdAt nostr.Timestamp) (*nostr.Event, error) {
	content, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal %s body: %w", typ, err)
	}
	tags := nostr.Tags{{"p", recipient}}
	if orderID != "" {
		tags = append(tags, nostr.Tag{"o", orderID})
	}
	tags = append(tags, nostr.Tag{"t", typ})
	ev := &nostr.Event{Kind: KindInner, CreatedAt: createdAt, Tags: tags, Content: string(content)}
	if err := ev.Sign(senderSecret); err != nil {
		return nil, fmt.Errorf("sign inner: %w", err)
	}
	return ev, nil
}

// Options make wrapping deterministic (test vectors only).
type Options struct {
	EphemeralSecret string // one-time key of the wrap; random if empty
	SealNonce       []byte // 32 byte NIP-44 nonces; random if nil
	WrapNonce       []byte
}

// Wrap seals a signed inner event for its recipient (the first p tag).
func Wrap(senderSecret string, inner *nostr.Event, opts Options) (*nostr.Event, error) {
	recipient := Recipient(inner)
	if recipient == "" {
		return nil, errors.New("inner event has no recipient")
	}
	innerJSON, err := json.Marshal(inner)
	if err != nil {
		return nil, err
	}
	sealContent, err := encrypt(senderSecret, recipient, string(innerJSON), opts.SealNonce)
	if err != nil {
		return nil, fmt.Errorf("encrypt seal: %w", err)
	}
	seal := &nostr.Event{Kind: KindSeal, CreatedAt: inner.CreatedAt, Tags: nostr.Tags{}, Content: sealContent}
	if err := seal.Sign(senderSecret); err != nil {
		return nil, fmt.Errorf("sign seal: %w", err)
	}
	sealJSON, err := json.Marshal(seal)
	if err != nil {
		return nil, err
	}
	eph := opts.EphemeralSecret
	if eph == "" {
		eph = nostr.GeneratePrivateKey()
	}
	wrapContent, err := encrypt(eph, recipient, string(sealJSON), opts.WrapNonce)
	if err != nil {
		return nil, fmt.Errorf("encrypt wrap: %w", err)
	}
	wrap := &nostr.Event{Kind: KindWrap, CreatedAt: inner.CreatedAt, Tags: nostr.Tags{{"p", recipient}}, Content: wrapContent}
	if err := wrap.Sign(eph); err != nil {
		return nil, fmt.Errorf("sign wrap: %w", err)
	}
	return wrap, nil
}

func encrypt(secret, recipient, plaintext string, nonce []byte) (string, error) {
	ck, err := nip44.GenerateConversationKey(recipient, secret)
	if err != nil {
		return "", err
	}
	if nonce != nil {
		return nip44.Encrypt(plaintext, ck, nip44.WithCustomNonce(nonce))
	}
	return nip44.Encrypt(plaintext, ck)
}

func decrypt(secret, sender, ciphertext string) (string, error) {
	ck, err := nip44.GenerateConversationKey(sender, secret)
	if err != nil {
		return "", err
	}
	return nip44.Decrypt(ciphertext, ck)
}

// Verify checks the id and the signature of an event.
func Verify(ev *nostr.Event) error {
	if !ev.CheckID() {
		return errors.New("event id does not match its content")
	}
	ok, err := ev.CheckSignature()
	if err != nil {
		return fmt.Errorf("event signature: %w", err)
	}
	if !ok {
		return errors.New("invalid event signature")
	}
	return nil
}

// Unwrap opens a gift wrap addressed to the owner of recipientSecret and returns the verified inner event.
func Unwrap(recipientSecret string, wrap *nostr.Event) (*nostr.Event, error) {
	me, err := nostr.GetPublicKey(recipientSecret)
	if err != nil {
		return nil, err
	}
	if wrap.Kind != KindWrap {
		return nil, fmt.Errorf("kind %d is not a gift wrap", wrap.Kind)
	}
	if err := Verify(wrap); err != nil {
		return nil, fmt.Errorf("wrap: %w", err)
	}
	if p := wrap.Tags.Find("p"); len(p) < 2 || p[1] != me {
		return nil, errors.New("wrap is not addressed to us")
	}
	sealJSON, err := decrypt(recipientSecret, wrap.PubKey, wrap.Content)
	if err != nil {
		return nil, fmt.Errorf("decrypt wrap: %w", err)
	}
	var seal nostr.Event
	if err := json.Unmarshal([]byte(sealJSON), &seal); err != nil {
		return nil, fmt.Errorf("parse seal: %w", err)
	}
	if seal.Kind != KindSeal {
		return nil, fmt.Errorf("seal has kind %d", seal.Kind)
	}
	if err := Verify(&seal); err != nil {
		return nil, fmt.Errorf("seal: %w", err)
	}
	innerJSON, err := decrypt(recipientSecret, seal.PubKey, seal.Content)
	if err != nil {
		return nil, fmt.Errorf("decrypt seal: %w", err)
	}
	var inner nostr.Event
	if err := json.Unmarshal([]byte(innerJSON), &inner); err != nil {
		return nil, fmt.Errorf("parse inner: %w", err)
	}
	if err := VerifyInner(&inner); err != nil {
		return nil, err
	}
	if inner.PubKey != seal.PubKey {
		return nil, errors.New("seal and inner event have different authors")
	}
	if Recipient(&inner) != me {
		return nil, errors.New("inner event is not addressed to us")
	}
	return &inner, nil
}

// VerifyInner checks a signed inner message, e.g. one quoted as evidence.
func VerifyInner(inner *nostr.Event) error {
	if inner.Kind != KindInner {
		return fmt.Errorf("inner event has kind %d", inner.Kind)
	}
	if err := Verify(inner); err != nil {
		return fmt.Errorf("inner: %w", err)
	}
	if Type(inner) == "" {
		return errors.New("inner event has no t tag")
	}
	return nil
}

func tagValue(ev *nostr.Event, name string) string {
	if t := ev.Tags.Find(name); len(t) >= 2 {
		return t[1]
	}
	return ""
}

// Recipient is the p tag of an inner event.
func Recipient(ev *nostr.Event) string { return tagValue(ev, "p") }

// OrderID is the o tag of an inner event.
func OrderID(ev *nostr.Event) string { return tagValue(ev, "o") }

// Type is the t tag of an inner event.
func Type(ev *nostr.Event) string { return tagValue(ev, "t") }
