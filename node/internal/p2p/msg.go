package p2p

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/core/protocol"
	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/giftwrap"
)

// ProtoMsg is the stream of the 1:1 messages (§4.2, §10).
const ProtoMsg = protocol.ID("/ps/msg/1.0.0")

// MaxMsgLine is the largest wrap line of /ps/msg/1.0.0 (64 KiB, without the newline).
const MaxMsgLine = 64 << 10

// MaxContactAddrs bounds the addresses taken from a profile's p2p or an order.request's reply_p2p.
const MaxContactAddrs = 8

// msgStreamTimeout bounds one /ps/msg exchange on the receiving side.
var msgStreamTimeout = 15 * time.Second

// MsgReply is the one line a receiver answers.
type MsgReply struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// MsgHandler takes a wrap received over /ps/msg/1.0.0 whose p tag names us; nil means taken.
type MsgHandler func(wrap *nostr.Event, from peer.ID) error

// HandleMessages serves /ps/msg/1.0.0 for the identity pubkey (hex): one wrap (kind 1059) per stream, answered
// with {"ok":true} or {"ok":false,"error":…}.
func (s *Service) HandleMessages(pubkey string, h MsgHandler) {
	s.h.SetStreamHandler(ProtoMsg, func(st network.Stream) { s.serveMsg(st, pubkey, h) })
}

func (s *Service) serveMsg(st network.Stream, pubkey string, h MsgHandler) {
	defer st.Close()
	_ = st.SetDeadline(time.Now().Add(msgStreamTimeout))
	reply := func(err error) {
		r := MsgReply{OK: err == nil}
		if err != nil {
			r.Error = err.Error()
		}
		data, _ := json.Marshal(r)
		_, _ = st.Write(append(data, '\n'))
	}
	line, err := readLine(st, MaxMsgLine)
	if err != nil {
		reply(err)
		return
	}
	wrap, err := CheckMsgLine(line, pubkey)
	if err != nil {
		reply(err)
		return
	}
	err = h(wrap, st.Conn().RemotePeer())
	if err != nil {
		s.log.Debug("message over p2p not taken", "peer", st.Conn().RemotePeer(), "err", err)
	}
	reply(err)
}

// CheckMsgLine decodes one /ps/msg/1.0.0 line: a kind 1059 wrap with a valid id and signature whose p tag is
// pubkey.
func CheckMsgLine(line []byte, pubkey string) (*nostr.Event, error) {
	var wrap nostr.Event
	if err := json.Unmarshal(line, &wrap); err != nil {
		return nil, errors.New("not a JSON event")
	}
	if wrap.Kind != giftwrap.KindWrap {
		return nil, fmt.Errorf("kind %d is not a wrap", wrap.Kind)
	}
	if p := wrap.Tags.Find("p"); len(p) < 2 || p[1] != pubkey {
		return nil, errors.New("wrap is not for this recipient")
	}
	if err := giftwrap.Verify(&wrap); err != nil {
		return nil, errors.New("bad wrap id or signature")
	}
	return &wrap, nil
}

// readLine reads up to the first newline; a line longer than max is an error.
func readLine(r io.Reader, max int) ([]byte, error) {
	br := bufio.NewReaderSize(io.LimitReader(r, int64(max)+2), 4096)
	var buf bytes.Buffer
	for {
		chunk, err := br.ReadSlice('\n')
		buf.Write(chunk)
		if buf.Len() > max+1 {
			return nil, fmt.Errorf("line larger than %d bytes", max)
		}
		if err == nil {
			return bytes.TrimRight(buf.Bytes(), "\r\n"), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && buf.Len() > 0 && buf.Len() <= max {
			return buf.Bytes(), nil // the sender closed without a newline
		}
		if errors.Is(err, io.EOF) {
			return nil, errors.New("no line")
		}
		return nil, err
	}
}

// Contact is a libp2p address of somebody (the p2p of a profile §3, the reply_p2p of an order.request §4.4).
type Contact struct {
	PeerID string   `json:"peer_id"`
	Addrs  []string `json:"addrs"`
}

// ParseContact checks a contact: a valid peer id, and at most MaxContactAddrs multiaddrs that end in that peer id
// (or in none). Addresses that do not parse, end in another peer id, or (unless allowPrivate) start with a
// loopback, private or link-local IP are left out; an empty list is fine (we may still reach the peer through
// our relays).
func ParseContact(c Contact, allowPrivate bool) (peer.ID, []ma.Multiaddr, error) {
	id, err := peer.Decode(c.PeerID)
	if err != nil {
		return "", nil, fmt.Errorf("peer_id %q: %w", c.PeerID, err)
	}
	var out []ma.Multiaddr
	for _, s := range c.Addrs {
		if len(out) == MaxContactAddrs {
			break
		}
		if len(s) > 1024 {
			continue
		}
		a, err := ma.NewMultiaddr(s)
		if err != nil {
			continue
		}
		transport, last := peer.SplitAddr(a)
		if transport == nil || (last != "" && last != id) {
			continue
		}
		if !allowPrivate && !manet.IsPublicAddr(transport) {
			continue
		}
		out = append(out, transport)
	}
	return id, out, nil
}

// SendMessage delivers one wrap to a peer over /ps/msg/1.0.0 (§4.2): it dials the contact's addresses (and
// circuits through our relays), writes the wrap as one line and reads the answer. nil means the peer took it.
func (s *Service) SendMessage(ctx context.Context, id peer.ID, addrs []ma.Multiaddr, wrap *nostr.Event) error {
	if id == s.h.ID() {
		return errors.New("that is us")
	}
	data, err := json.Marshal(wrap)
	if err != nil {
		return err
	}
	if len(data) > MaxMsgLine {
		return fmt.Errorf("wrap is %d bytes, over the %d of /ps/msg", len(data), MaxMsgLine)
	}
	if len(addrs) > 0 {
		s.h.Peerstore().AddAddrs(id, addrs, peerstore.TempAddrTTL)
	}
	if err := s.h.ConnectPeer(ctx, id); err != nil {
		return err
	}
	st, err := s.h.NewStream(network.WithAllowLimitedConn(ctx, "ps-msg"), id, ProtoMsg)
	if err != nil {
		return fmt.Errorf("open msg stream: %w", err)
	}
	defer st.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = st.SetDeadline(dl)
	}
	if _, err := st.Write(append(data, '\n')); err != nil {
		_ = st.Reset()
		return fmt.Errorf("write msg: %w", err)
	}
	_ = st.CloseWrite()
	line, err := readLine(st, 4096)
	if err != nil {
		return fmt.Errorf("read msg answer: %w", err)
	}
	var r MsgReply
	if err := json.Unmarshal(line, &r); err != nil {
		return fmt.Errorf("msg answer %q is not JSON", line)
	}
	if !r.OK {
		return fmt.Errorf("peer refused the message: %s", r.Error)
	}
	return nil
}
