package vectors

import (
	"encoding/hex"
	"encoding/json"
	"strconv"

	p2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/keys"
	"github.com/pad01g/proxy-shopping-go/node/internal/proto"
	"github.com/pad01g/proxy-shopping-go/node/internal/trust"
)

// VectorTime is the created_at of the signed events of the p2p vectors.
const VectorTime = 1790000000

// addLibp2p: the libp2p key of §1 (m/7333'/0'/0', secp256k1) of the abandon mnemonic and its peer id.
func (f *File) addLibp2p(abandon *keys.Set) error {
	k, err := abandon.Libp2pKey()
	if err != nil {
		return err
	}
	raw, err := k.Raw()
	if err != nil {
		return err
	}
	pubRaw, err := k.GetPublic().Raw()
	if err != nil {
		return err
	}
	pubProto, err := p2pcrypto.MarshalPublicKey(k.GetPublic())
	if err != nil {
		return err
	}
	id, err := abandon.PeerID()
	if err != nil {
		return err
	}
	f.Libp2p = map[string]any{
		"mnemonic": Abandon, "path": "m/7333'/0'/0'",
		"secret": hex.EncodeToString(raw), "pubkey_compressed": hex.EncodeToString(pubRaw),
		"pubkey_protobuf": hex.EncodeToString(pubProto), "peer_id": id.String(),
	}
	return nil
}

// signAt signs an event with a fixed created_at.
func signAt(secret string, kind int, tags nostr.Tags, content string) (*nostr.Event, error) {
	ev := &nostr.Event{Kind: kind, CreatedAt: VectorTime, Tags: tags, Content: content}
	return ev, ev.Sign(secret)
}

func jsonString(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

// addP2P: a list bundle of §2.6 (delegation with list_url, list with p2p_relays, shopper profile with circuit
// addresses, inbox relays), a reply_p2p of §4.4 and the lines of /ps/msg/1.0.0 (§10) for the gift_wrap vector.
func (f *File) addP2P(sets map[string]*keys.Set, wrap *nostr.Event) error {
	coord, op, shopper, escrow, user, relay := sets["coordinator-1"], sets["operator-1"], sets["shopper-1"], sets["escrow-1"], sets["user-1"], sets["relay-p2p"]
	relayID, err := relay.PeerID()
	if err != nil {
		return err
	}
	shopperID, err := shopper.PeerID()
	if err != nil {
		return err
	}
	userID, err := user.PeerID()
	if err != nil {
		return err
	}
	relayAddr := "/dns4/relay.example/tcp/443/tls/ws/p2p/" + relayID.String()
	const listURL = "https://operator.example/ps-lab/bundle.json"
	v := strconv.Itoa(VectorTime)
	del, err := signAt(coord.NostrSecretHex(), trust.KindDelegation, nostr.Tags{
		{"d", op.NostrPubHex()}, {"v", v}, {"network", "ps-lab"}, {"p", op.NostrPubHex()}, {"revoked", "false"}, {"list_url", listURL},
	}, `{"note":"vector"}`)
	if err != nil {
		return err
	}
	list := trust.List{
		Network: "ps-lab", Name: "vector operator", Regions: []string{"JP-13"},
		Relays: []trust.Relay{{URL: "wss://relay-1.test", RetentionDays: 30}}, P2PRelays: []string{relayAddr},
		Entries: []trust.Entry{{Region: "JP-13", Shopper: shopper.NostrPubHex(), Escrow: escrow.NostrPubHex(), Shops: []string{"*"},
			Payments: []string{"btc-signet"}, Tags: []string{}, EscrowSLADays: 14}},
		ReportTo: op.NostrPubHex(),
	}
	listEv, err := signAt(op.NostrSecretHex(), trust.KindList, nostr.Tags{{"d", "ps-lab"}, {"v", v}, {"network", "ps-lab"}}, jsonString(list))
	if err != nil {
		return err
	}
	profile := trust.ShopperProfile{
		Name: "shopper-1", Payments: []string{"btc-signet"}, Currencies: []string{"JPY"}, CashRegions: []string{},
		DeliveryDays: 5, BTCAddress: shopper.WalletAddress(),
		P2P: &trust.P2PInfo{PeerID: shopperID.String(), Addrs: []string{relayAddr + "/p2p-circuit/p2p/" + shopperID.String()}},
	}
	profEv, err := signAt(shopper.NostrSecretHex(), trust.KindShopperProfile, nostr.Tags{{"d", "ps-lab"}, {"v", v}, {"network", "ps-lab"}}, jsonString(profile))
	if err != nil {
		return err
	}
	inbox, err := signAt(shopper.NostrSecretHex(), trust.KindInboxRelays, nostr.Tags{{"relay", "wss://relay-1.test"}, {"relay", "wss://relay-2.test"}}, "")
	if err != nil {
		return err
	}
	for _, ev := range []*nostr.Event{del, listEv, profEv, inbox} {
		if err := trust.Validate(ev); err != nil {
			return err
		}
	}
	replyP2P := proto.P2PContact{PeerID: userID.String(), Addrs: []string{relayAddr + "/p2p-circuit/p2p/" + userID.String()}}
	if err := replyP2P.Validate(); err != nil {
		return err
	}
	wrapLine, err := json.Marshal(wrap)
	if err != nil {
		return err
	}
	f.P2P = map[string]any{
		"list_url": listURL,
		"bundle":   trust.MakeBundle([]*nostr.Event{inbox, profEv, listEv, del}),
		"reply_p2p": map[string]any{
			"user": "user-1", "contact": replyP2P,
		},
		"msg": map[string]any{
			"protocol":     "/ps/msg/1.0.0",
			"request_line": string(wrapLine) + "\n",
			"ok_line":      `{"ok":true}` + "\n",
			"error_line":   `{"ok":false,"error":"wrap is not for this recipient"}` + "\n",
			"max_line":     64 << 10,
		},
	}
	return nil
}
