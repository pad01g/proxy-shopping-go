package p2p

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/giftwrap"
)

func testWrap(t *testing.T, from, to string) *nostr.Event {
	t.Helper()
	toPub, _ := nostr.GetPublicKey(to)
	inner, err := giftwrap.NewInner(from, toPub, "00112233445566778899aabbccddeeff", "chat", map[string]string{"text": "hi"}, nostr.Now())
	if err != nil {
		t.Fatal(err)
	}
	w, err := giftwrap.Wrap(from, inner, giftwrap.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// /ps/msg/1.0.0 (§4.2, §10) between a listening node and one behind a relay it learnt at run time (p2p_relays):
// the wrap arrives over the circuit, the answer is ok; a wrap for somebody else and a refusal of the handler are
// answered ok:false; a peer that does not speak the protocol is an error.
func TestMsgStreamThroughRelay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	relay := startNode(t, ctx, Options{Listen: []string{"/ip4/127.0.0.1/tcp/0", "/ip4/127.0.0.1/tcp/0/ws"}, Reachability: "public", RelayService: true}, "relay")
	relayAddr := relay.h.FullAddrs()[0]

	a := startNode(t, ctx, Options{Listen: []string{"/ip4/127.0.0.1/tcp/0"}, Bootstrap: []string{relayAddr}}, "shopper")
	b := startNode(t, ctx, Options{Reachability: "private"}, "escrow")
	if n := b.h.AddRelays([]string{relayAddr, relayAddr, "/not/an/addr"}); n != 1 {
		t.Fatalf("added %d relays", n)
	}
	select {
	case <-b.h.Changes():
	case <-ctx.Done():
		t.Fatal("no reservation signalled")
	}
	circuits := b.h.CircuitAddrs()
	if len(circuits) == 0 || !strings.Contains(circuits[0], "/p2p-circuit/p2p/"+b.h.ID().String()) {
		t.Fatalf("circuit addrs %v", circuits)
	}

	aSecret, bSecret := nostr.GeneratePrivateKey(), nostr.GeneratePrivateKey()
	bPub, _ := nostr.GetPublicKey(bSecret)
	var mu sync.Mutex
	var got []*nostr.Event
	refuse := false
	b.svc.HandleMessages(bPub, func(w *nostr.Event, from peer.ID) error {
		mu.Lock()
		defer mu.Unlock()
		if from != a.h.ID() {
			t.Errorf("message from %s", from)
		}
		if refuse {
			return errors.New("rate limited")
		}
		got = append(got, w)
		return nil
	})

	id, addrs, err := ParseContact(Contact{PeerID: b.h.ID().String(), Addrs: circuits}, true)
	if err != nil || len(addrs) != len(circuits) {
		t.Fatalf("contact: %v %v", err, addrs)
	}
	w := testWrap(t, aSecret, bSecret)
	sctx, scancel := context.WithTimeout(ctx, 10*time.Second)
	err = a.svc.SendMessage(sctx, id, addrs, w)
	scancel()
	if err != nil {
		t.Fatalf("send over the circuit: %v", err)
	}
	mu.Lock()
	if len(got) != 1 || got[0].ID != w.ID {
		t.Fatalf("received %v", got)
	}
	refuse = true
	mu.Unlock()

	if err := a.svc.SendMessage(ctx, id, addrs, testWrap(t, aSecret, nostr.GeneratePrivateKey())); err == nil || !strings.Contains(err.Error(), "not for this recipient") {
		t.Fatalf("wrap for somebody else: %v", err)
	}
	if err := a.svc.SendMessage(ctx, id, addrs, testWrap(t, aSecret, bSecret)); err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("refused wrap: %v", err)
	}
	// the relay does not serve /ps/msg
	if err := a.svc.SendMessage(ctx, relay.h.ID(), nil, w); err == nil {
		t.Fatal("a peer without /ps/msg took the message")
	}
}

// One line in, one line out; the line is at most 64 KiB; the p tag must name the recipient.
func TestMsgLine(t *testing.T) {
	aSecret, bSecret := nostr.GeneratePrivateKey(), nostr.GeneratePrivateKey()
	bPub, _ := nostr.GetPublicKey(bSecret)
	w := testWrap(t, aSecret, bSecret)
	line, _ := json.Marshal(w)
	if got, err := CheckMsgLine(line, bPub); err != nil || got.ID != w.ID {
		t.Fatalf("good line: %v", err)
	}
	if _, err := CheckMsgLine(line, strings.Repeat("0", 64)); err == nil {
		t.Fatal("wrong recipient accepted")
	}
	tampered := *w
	tampered.Content += "x"
	data, _ := json.Marshal(&tampered)
	if _, err := CheckMsgLine(data, bPub); err == nil {
		t.Fatal("tampered wrap accepted")
	}
	notWrap := *w
	notWrap.Kind = 1
	data, _ = json.Marshal(&notWrap)
	if _, err := CheckMsgLine(data, bPub); err == nil {
		t.Fatal("kind 1 accepted")
	}

	if l, err := readLine(bytes.NewReader(append(line, '\n', 'x')), MaxMsgLine); err != nil || !bytes.Equal(l, line) {
		t.Fatalf("readLine: %v", err)
	}
	exact := bytes.Repeat([]byte("a"), MaxMsgLine)
	if l, err := readLine(bytes.NewReader(append(exact, '\n')), MaxMsgLine); err != nil || len(l) != MaxMsgLine {
		t.Fatalf("a 64 KiB line: %v %d", err, len(l))
	}
	if _, err := readLine(bytes.NewReader(append(append(exact, 'a'), '\n')), MaxMsgLine); err == nil {
		t.Fatal("a line over 64 KiB accepted")
	}
	if _, err := readLine(bytes.NewReader(nil), MaxMsgLine); err == nil {
		t.Fatal("empty stream accepted")
	}
}

// reply_p2p and profile addresses: the peer id must parse, addresses of other peers and (outside the lab) private
// addresses are left out.
func TestParseContact(t *testing.T) {
	const id = "16Uiu2HAmFngaVFK4D5fix5qN2c6guh3LNyBkmuB42vTFW9aaeDZh"
	const relayID = "16Uiu2HAmMnvsz9miPEy1kqM9eD6hEU1hSKpS2Ze9q6ko2YUPATbR"
	c := Contact{PeerID: id, Addrs: []string{
		"/dns4/relay.example/tcp/443/tls/ws/p2p/" + relayID + "/p2p-circuit/p2p/" + id,
		"/ip4/192.168.1.5/tcp/4001/p2p/" + id,
		"/ip4/8.8.8.8/tcp/4001",
		"/ip4/8.8.8.8/tcp/4001/p2p/" + relayID, // another peer
		"garbage",
	}}
	_, addrs, err := ParseContact(c, false)
	if err != nil || len(addrs) != 2 {
		t.Fatalf("public: %v %v", err, addrs)
	}
	if !strings.HasSuffix(addrs[0].String(), "/p2p-circuit") {
		t.Fatalf("the circuit address loses only its last /p2p: %s", addrs[0])
	}
	if _, addrs, _ := ParseContact(c, true); len(addrs) != 3 {
		t.Fatalf("lab: %v", addrs)
	}
	if _, _, err := ParseContact(Contact{PeerID: "nope"}, true); err == nil {
		t.Fatal("bad peer id accepted")
	}
}
