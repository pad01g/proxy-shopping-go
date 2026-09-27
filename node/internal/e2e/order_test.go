//go:build integration

package e2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/amount"
	"github.com/pad01g/proxy-shopping-go/node/internal/btc"
	"github.com/pad01g/proxy-shopping-go/node/internal/contract"
	"github.com/pad01g/proxy-shopping-go/node/internal/delivery"
	"github.com/pad01g/proxy-shopping-go/node/internal/escrow"
	"github.com/pad01g/proxy-shopping-go/node/internal/keys"
	"github.com/pad01g/proxy-shopping-go/node/internal/node"
	"github.com/pad01g/proxy-shopping-go/node/internal/proto"
	"github.com/pad01g/proxy-shopping-go/node/internal/shopper"
	"github.com/pad01g/proxy-shopping-go/node/internal/testutil"
	"github.com/pad01g/proxy-shopping-go/node/internal/trust"
)

const network = "ps-e2e"

type env struct {
	chain         *chain
	relays        []string
	shopperAdmin  string
	escrowAdmin   string
	bot           *fakeBot
	shopURL       string
	user          *user
	shopper, escr *keys.Set
	operator      *keys.Set
}

func btcChain(esploraURL string) string {
	return fmt.Sprintf("chain:\n  btc: {network: signet, esplora: %q}\n", esploraURL)
}

func nodeYAML(role, name string, adminPort, p2pPort int, relays []string, chainYAML, ca, dataDir, extra string, bootstrap string) string {
	quoted := make([]string, len(relays))
	for i, r := range relays {
		quoted[i] = strconv.Quote(r)
	}
	boot := "[]"
	if bootstrap != "" {
		boot = "[" + strconv.Quote(bootstrap) + "]"
	}
	return fmt.Sprintf(`role: %s
name: %s
network: %s
mnemonic_file: %s
data_dir: %s
admin: {listen: "127.0.0.1:%d", token: "lab"}
tls: {extra_ca: %q}
nostr: {relays: [%s], k: 2, allow_private_relays: true}
p2p: {listen: ["/ip4/127.0.0.1/tcp/%d"], bootstrap: %s, reachability: public}
trust: {coordinators: [%q]}
%sfx:
  sources:
    - {type: static, rates: {"BTC/USD": "100000", "USD/JPY": "150", "USDC/USD": "1"}}
%s`, role, name, network, keyDir+"/"+name+".mnemonic", dataDir, adminPort, ca, strings.Join(quoted, ", "), p2pPort, boot,
		coordinatorPub, chainYAML, extra)
}

var coordinatorPub string

func setup(t *testing.T, ctx context.Context) *env {
	t.Helper()
	node.ProfileInterval = time.Minute
	coord := labKeys(t, "coordinator-1")
	coordinatorPub = coord.NostrPubHex()
	e := &env{chain: startChain(t, ctx), relays: []string{testutil.StartRelay(t), testutil.StartRelay(t)}}
	e.shopper, e.escr, e.operator = labKeys(t, "shopper-1"), labKeys(t, "escrow-1"), labKeys(t, "operator-1")
	shop, ca := fakeShop(t)
	e.shopURL = shop.URL + "/"
	bot, botURL := startBot(t)
	e.bot = bot

	shopperP2P := freePort(t)
	shopperID, _ := e.shopper.PeerID()
	shopperCfg := nodeYAML("shopper", "shopper-1", freePort(t), shopperP2P, e.relays, btcChain(e.chain.esplora), ca, t.TempDir(), fmt.Sprintf(`shopper:
  bot_url: %q
  payments: [btc-signet]
  currencies: [JPY, USD]
  cash_regions: [JP-27]
  fee: {bps: 500, min: {amount: "300", currency: JPY}}
  max_order: {amount: "200000", currency: JPY}
  delivery_days: 5
  risk: {allowlist: [127.0.0.1], known_gateways: [cardgw.test], threshold: 70}
  timelock: {btc_t1_blocks: 20, btc_t2_blocks: 30, evm_t1_seconds: 3600, evm_t2_seconds: 7200}
  confirmations: 1
  tracking_poll_seconds: 1
  accept_rulings: always
  min_t1_remaining_seconds: 1800
  allow_private_shops: true
`, botURL), "")
	e.shopperAdmin = runNode(t, ctx, shopperCfg)
	escrowCfg := nodeYAML("escrow", "escrow-1", freePort(t), freePort(t), e.relays, btcChain(e.chain.esplora), ca, t.TempDir(), `escrow:
  upfront_fee: {bps: 50, min_sats: "1000", min_usdc: "0.50"}
  dispute_fee_bps: 200
`, fmt.Sprintf("/ip4/127.0.0.1/tcp/%d/p2p/%s", shopperP2P, shopperID))
	e.escrowAdmin = runNode(t, ctx, escrowCfg)

	e.seedTrust(t, ctx, coord, "btc-signet")
	e.user = newUser(t, ctx, e.relays)
	return e
}

// seedTrust posts a delegation and a list naming shopper-1 × escrow-1 for Tokyo and waits until both nodes
// know the list and the shopper knows the escrow profile.
func (e *env) seedTrust(t *testing.T, ctx context.Context, coord *keys.Set, payment string) {
	t.Helper()
	// the coordinator delegates to the operator, whose list names shopper-1 × escrow-1 for Tokyo
	del, err := trust.NewDelegation(coord.NostrSecretHex(), e.operator.NostrPubHex(), network, 1, false, "e2e")
	if err != nil {
		t.Fatal(err)
	}
	list, err := trust.NewList(e.operator.NostrSecretHex(), 1, &trust.List{
		Network: network, Name: "e2e operator", Regions: []string{"JP-13"},
		Entries: []trust.Entry{{Region: "JP-13", Shopper: e.shopper.NostrPubHex(), Escrow: e.escr.NostrPubHex(),
			Shops: []string{"*"}, Payments: []string{payment}, EscrowSLADays: 14}},
		ReportTo: e.operator.NostrPubHex(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if code := admin(t, "POST", e.shopperAdmin+"/events", []*nostr.Event{del, list}, &[]map[string]any{}); code != 200 {
		t.Fatalf("POST /events: %d", code)
	}
	// both nodes see the list (the escrow through Nostr or gossip) and the shopper sees the escrow profile
	waitUntil(t, ctx, "escrow to learn the list", func() bool {
		var st node.Status
		admin(t, "GET", e.escrowAdmin+"/status", nil, &st)
		return st.Trust[e.operator.NostrPubHex()] == 1
	})
	waitUntil(t, ctx, "shopper to learn the escrow profile", func() bool {
		var tr struct {
			Events []*nostr.Event `json:"events"`
		}
		admin(t, "GET", e.shopperAdmin+"/trust", nil, &tr)
		for _, ev := range tr.Events {
			if ev.Kind == trust.KindEscrowProfile && ev.PubKey == e.escr.NostrPubHex() {
				return true
			}
		}
		return false
	})
}

func newOrderID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// order is the user's view of one order.
type order struct {
	id      string
	quote   proto.OrderQuote
	quoteEv *nostr.Event
	req     proto.OrderRequest
	esc     btc.Escrow
	prev    btc.Outpoint
	events  []*nostr.Event
	key     [32]byte
	// keyForEscrow is key_for_escrow of order.escrow_key
	keyForEscrow string
}

// placeAndFund runs request → quote → accept → funded (and escrow.notice) and waits until the shopper bought.
func (e *env) placeAndFund(t *testing.T, ctx context.Context, sku string) *order {
	// lab prices (docs/lab.md): shipping 800 JPY, fee 5% with a 300 JPY minimum
	expect := map[string][2]string{"A-100": {"3200", "300"}, "A-200": {"12000", "640"}}[sku]
	price, shopperFee := expect[0], expect[1]
	t.Helper()
	u := e.user
	o := &order{id: newOrderID()}
	k, _ := delivery.NewKey()
	o.key = k
	addr := delivery.Address{Name: "山田 太郎", PostalCode: "160-0022", Address: "東京都新宿区新宿3-1-1", Phone: "03-0000-0000"}
	ct, _ := delivery.Seal(k, nil, o.id, addr)
	kShopper, _ := delivery.WrapKey(u.keys.NostrSecretHex(), e.shopper.NostrPubHex(), k)
	kEscrow, _ := delivery.WrapKey(u.keys.NostrSecretHex(), e.escr.NostrPubHex(), k)
	o.keyForEscrow = kEscrow
	userKey, _ := u.keys.OrderKey(o.id)
	proof, err := keys.SignKeyProofBTC(userKey, o.id, u.keys.NostrPubHex())
	if err != nil {
		t.Fatal(err)
	}
	o.req = proto.OrderRequest{
		ShopURL: e.shopURL, ShopRegion: "JP-13-13104", Items: []proto.Item{{SKU: sku, Qty: 1}}, Payment: proto.AssetBTC,
		Escrow: e.escr.NostrPubHex(), Operator: e.operator.NostrPubHex(), Coordinator: coordinatorPub,
		Delivery:      proto.Delivery{Ciphertext: ct, KeyForShopper: kShopper, KeyForEscrowSHA256: proto.EscrowKeyHash(kEscrow)},
		KeyProof:      proof,
		UserBTCPubkey: hex.EncodeToString(userKey.PubKey().SerializeCompressed()), UserBTCAddress: u.keys.WalletAddress(),
		Relays: e.relays,
	}
	reqEv := u.send(t, ctx, e.shopper.NostrPubHex(), o.id, proto.TypeOrderRequest, o.req)
	// key_for_escrow travels apart from the request, which only commits to it (§4.4)
	u.send(t, ctx, e.shopper.NostrPubHex(), o.id, proto.TypeOrderEscrowKey, proto.EscrowKey{KeyForEscrow: kEscrow})
	qm := u.wait(t, ctx, o.id, proto.TypeOrderQuote, &o.quote)
	o.quoteEv = qm.Inner
	if !o.quote.Accept {
		t.Fatalf("quote rejected: %s %s", o.quote.RejectReason, o.quote.Detail)
	}

	// the user checks the quote: price, rate and the escrow address it recomputes itself
	if o.quote.Price.Items.Amount != price || o.quote.Price.Shipping.Amount != "800" || o.quote.FX.Rate != "15000000" {
		t.Fatalf("quote price %+v fx %+v", o.quote.Price, o.quote.FX)
	}
	if o.esc, err = contract.BTCEscrow(&o.req, &o.quote); err != nil {
		t.Fatal(err)
	}
	xpub, _ := e.escr.EscrowXpub()
	ek, _ := keys.EscrowChildPubKey(xpub, o.id)
	if hex.EncodeToString(ek.SerializeCompressed()) != o.quote.EscrowBTCPubkey {
		t.Fatal("quoted escrow key is not derived from the escrow xpub")
	}
	if a, _ := o.esc.Address(); a != o.quote.EscrowAddress {
		t.Fatalf("escrow address %s, recomputed %s", o.quote.EscrowAddress, a)
	}
	total := new(big.Rat).Add(mustRat(price), mustRat("800"))
	total.Add(total, mustRat(shopperFee))
	want, _ := amount.LockAmount(total, mustRat("15000000"), 8, 1000)
	if o.quote.LockAmount != want.String() || o.quote.Price.ShopperFee.Amount != shopperFee {
		t.Fatalf("lock %s (want %s), fee %s", o.quote.LockAmount, want, o.quote.Price.ShopperFee.Amount)
	}

	accEv := u.send(t, ctx, e.shopper.NostrPubHex(), o.id, proto.TypeOrderAccept, proto.OrderAccept{QuoteID: o.quoteEv.ID})
	lock, _ := strconv.ParseInt(o.quote.LockAmount, 10, 64)
	fee, _ := strconv.ParseInt(o.quote.EscrowUpfrontFee, 10, 64)
	txid, vout := e.chain.fund(t, ctx, o.quote.EscrowAddress, lock, o.quote.EscrowBTCFeeAddress, fee)
	o.prev = btc.Outpoint{TxID: txid, Vout: vout, Amount: lock}
	fundEv := u.send(t, ctx, e.shopper.NostrPubHex(), o.id, proto.TypeOrderFunded, proto.OrderFunded{Asset: proto.AssetBTC, TxID: txid, Vout: &vout, Amount: o.quote.LockAmount, FeeTxID: txid})
	o.events = []*nostr.Event{reqEv, o.quoteEv, accEv, fundEv}
	u.send(t, ctx, e.escr.NostrPubHex(), o.id, proto.TypeEscrowNotice, proto.EscrowNotice{Request: reqEv, Quote: o.quoteEv, Accept: accEv, Funded: fundEv})

	var purchased proto.OrderPurchased
	u.wait(t, ctx, o.id, proto.TypeOrderPurchased, &purchased)
	if purchased.ShopOrderID != "SS-"+o.id[:8] {
		t.Fatalf("purchased %+v", purchased)
	}
	// the bot got the decrypted address
	p, ok := e.bot.purchase(o.id)
	if !ok || !strings.Contains(fmt.Sprint(p.Shipping), "山田 太郎") || p.PaymentRef != "card:default" || p.RequestID != o.id {
		t.Fatalf("bot purchase %+v", p)
	}
	if e.bot.duplicated() {
		t.Fatal("the node bought an order twice")
	}
	return o
}

func mustRat(s string) *big.Rat {
	r, err := amount.Parse(s)
	if err != nil {
		panic(err)
	}
	return r
}

func (e *env) shopperOrder(t *testing.T, id string) shopper.Order {
	t.Helper()
	var o shopper.Order
	admin(t, "GET", e.shopperAdmin+"/orders/"+id, nil, &o)
	return o
}

func (e *env) mempoolOrChain(t *testing.T, ctx context.Context, txid string) *btc.Tx {
	t.Helper()
	cl := btc.NewEsplora(e.chain.esplora, nil)
	var tx *btc.Tx
	waitUntil(t, ctx, "transaction "+txid, func() bool {
		var err error
		tx, err = cl.Tx(ctx, txid)
		return err == nil
	})
	return tx
}

func paid(tx *btc.Tx, addr string) int64 {
	var n int64
	for _, o := range tx.Vout {
		if o.ScriptPubKeyAddress == addr {
			n += o.Value
		}
	}
	return n
}

// TestOrderFlows runs three BTC orders through the nodes: a released one, a disputed one ruled by the escrow and
// countersigned by the shopper, and one the shopper claims through the T1 branch.
func TestOrderFlows(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	e := setup(t, ctx)
	u := e.user

	// libp2p status query from the escrow to the shopper (the escrow bootstraps to it)
	var p2pStatus struct {
		Status struct {
			Role string `json:"role"`
		} `json:"status"`
	}
	shopperID, _ := e.shopper.PeerID()
	waitUntil(t, ctx, "p2p status", func() bool {
		return admin(t, "POST", e.escrowAdmin+"/p2p/status", map[string]string{"peer_id": shopperID.String()}, &p2pStatus) == 200
	})
	if p2pStatus.Status.Role != "shopper" {
		t.Fatalf("p2p status %+v", p2pStatus)
	}

	t.Run("release", func(t *testing.T) {
		o := e.placeAndFund(t, ctx, "A-100")
		var ship proto.OrderShipping
		u.wait(t, ctx, o.id, proto.TypeOrderShipping, &ship)
		waitUntil(t, ctx, "delivered", func() bool { return e.shopperOrder(t, o.id).ShipStatus == "delivered" })

		userKey, _ := u.keys.OrderKey(o.id)
		reserve, _ := strconv.ParseInt(o.quote.PayoutFeeReserve, 10, 64)
		p, err := o.esc.NewSpend(o.prev, []btc.Output{{Address: o.quote.ShopperBTCAddress, Amount: o.prev.Amount - reserve}}, btc.PathMultisig)
		if err != nil {
			t.Fatal(err)
		}
		if err := btc.Sign(p, userKey); err != nil {
			t.Fatal(err)
		}
		b64, _ := btc.EncodePSBT(p)
		u.send(t, ctx, e.shopper.NostrPubHex(), o.id, proto.TypeOrderRelease, proto.Release{Asset: proto.AssetBTC, PSBT: b64})
		var done proto.TxRef
		u.wait(t, ctx, o.id, proto.TypeOrderCompleted, &done)
		tx := e.mempoolOrChain(t, ctx, done.TxID)
		if got := paid(tx, o.quote.ShopperBTCAddress); got != o.prev.Amount-reserve {
			t.Fatalf("shopper paid %d", got)
		}
		if st := e.shopperOrder(t, o.id).State; st != shopper.StateCompleted {
			t.Fatalf("shopper state %s", st)
		}
	})

	t.Run("dispute", func(t *testing.T) {
		o := e.placeAndFund(t, ctx, "A-200")
		// the user says the item was wrong and asks the escrow; the shopper gets a copy
		open := proto.DisputeOpen{Claim: proto.ClaimWrongItem, Text: "got a different teapot",
			Evidence: proto.DisputeEvidence{Messages: o.events, DeliveryKeyForEscrow: o.keyForEscrow}}
		u.send(t, ctx, e.escr.NostrPubHex(), o.id, proto.TypeDisputeOpen, open)
		u.send(t, ctx, e.shopper.NostrPubHex(), o.id, proto.TypeDisputeOpen, open)

		var c escrow.Case
		waitUntil(t, ctx, "the escrow to collect the shopper's evidence", func() bool {
			admin(t, "GET", e.escrowAdmin+"/cases/"+o.id, nil, &c)
			return len(c.Evidence[e.shopper.NostrPubHex()]) > 0
		})
		if c.State != escrow.CaseOpen || !c.FeePaid || c.DeliveryAddress == nil || c.DeliveryAddress.Name != "山田 太郎" {
			t.Fatalf("case %+v", c)
		}
		// the shopper hands over the user's order.escrow_key (a signed message among its evidence)
		ev := c.Evidence[e.shopper.NostrPubHex()][0]
		if ev.DeliveryKeyForEscrow != o.keyForEscrow || len(ev.Messages) < 5 || len(ev.PurchaseEvidence) == 0 {
			t.Fatalf("shopper evidence %+v", ev)
		}
		// a ruling is given once
		defer func() {
			if code := admin(t, "POST", e.escrowAdmin+"/cases/"+o.id+"/rule", map[string]string{"user": "1", "shopper": "1"}, nil); code != 409 {
				t.Errorf("second ruling answered %d", code)
			}
		}()

		reserve, _ := strconv.ParseInt(o.quote.PayoutFeeReserve, 10, 64)
		total := o.prev.Amount - reserve
		fee := total * 200 / 10000
		shopperShare := (total - fee) / 2
		userShare := total - fee - shopperShare
		var bad map[string]string
		if code := admin(t, "POST", e.escrowAdmin+"/cases/"+o.id+"/rule", map[string]string{"user": "1", "shopper": "1"}, &bad); code != 400 {
			t.Fatalf("inconsistent split answered %d", code)
		}
		var ruling proto.Ruling
		if code := admin(t, "POST", e.escrowAdmin+"/cases/"+o.id+"/rule", map[string]string{
			"user": fmt.Sprint(userShare), "shopper": fmt.Sprint(shopperShare), "reason": "half each",
		}, &ruling); code != 200 {
			t.Fatalf("rule: %d", code)
		}
		if ruling.Split.EscrowFee != fmt.Sprint(fee) {
			t.Fatalf("ruling %+v", ruling.Split)
		}
		u.wait(t, ctx, o.id, proto.TypeDisputeRuling, nil)
		var cs proto.TxRef
		u.wait(t, ctx, o.id, proto.TypeDisputeCountersigned, &cs)
		tx := e.mempoolOrChain(t, ctx, cs.TxID)
		if paid(tx, o.req.UserBTCAddress) != userShare || paid(tx, o.quote.ShopperBTCAddress) != shopperShare || paid(tx, o.quote.EscrowBTCFeeAddress) != fee {
			t.Fatalf("ruling payout %+v", tx.Vout)
		}
		waitUntil(t, ctx, "case closed", func() bool {
			admin(t, "GET", e.escrowAdmin+"/cases/"+o.id, nil, &c)
			return c.State == escrow.CaseClosed
		})
	})

	t.Run("timelock", func(t *testing.T) {
		o := e.placeAndFund(t, ctx, "A-100")
		waitUntil(t, ctx, "delivered", func() bool { return e.shopperOrder(t, o.id).ShipStatus == "delivered" })
		// the user never releases; after T1 the shopper takes the money alone
		height, _ := e.chain.rpc.GetBlockCount(ctx)
		if height < o.quote.Timelock.T1 {
			if _, err := e.chain.faucet.Mine(ctx, int(o.quote.Timelock.T1-height)); err != nil {
				t.Fatal(err)
			}
		}
		var done proto.TxRef
		u.wait(t, ctx, o.id, proto.TypeOrderCompleted, &done)
		tx := e.mempoolOrChain(t, ctx, done.TxID)
		if tx.Locktime != uint32(o.quote.Timelock.T1) || paid(tx, o.quote.ShopperBTCAddress) == 0 {
			t.Fatalf("claim tx %+v", tx)
		}
		if st := e.shopperOrder(t, o.id).State; st != shopper.StateClaimed {
			t.Fatalf("state %s", st)
		}
	})

	t.Run("rejections", func(t *testing.T) {
		// an escrow outside the list is refused before any risk or price work
		oid := newOrderID()
		request := func(oid string) proto.OrderRequest {
			dk, _ := delivery.NewKey()
			ct, _ := delivery.Seal(dk, nil, oid, delivery.Address{Name: "A", PostalCode: "1", Address: "x", Phone: "0"})
			kShopper, _ := delivery.WrapKey(u.keys.NostrSecretHex(), e.shopper.NostrPubHex(), dk)
			k, _ := u.keys.OrderKey(oid)
			proof, _ := keys.SignKeyProofBTC(k, oid, u.keys.NostrPubHex())
			return proto.OrderRequest{ShopURL: e.shopURL, ShopRegion: "JP-13-13104", Items: []proto.Item{{SKU: "A-100", Qty: 1}},
				Payment: proto.AssetBTC, Escrow: e.operator.NostrPubHex(), Operator: e.operator.NostrPubHex(),
				Delivery: proto.Delivery{Ciphertext: ct, KeyForShopper: kShopper, KeyForEscrowSHA256: proto.EscrowKeyHash("x")},
				KeyProof: proof, UserBTCAddress: u.keys.WalletAddress(), UserBTCPubkey: hex.EncodeToString(k.PubKey().SerializeCompressed())}
		}
		req := request(oid)
		u.send(t, ctx, e.shopper.NostrPubHex(), oid, proto.TypeOrderRequest, req)
		var q proto.OrderQuote
		u.wait(t, ctx, oid, proto.TypeOrderQuote, &q)
		if q.Accept || q.RejectReason != proto.RejectTrust {
			t.Fatalf("quote %+v", q)
		}
		// a region the list does not cover
		oid2 := newOrderID()
		req = request(oid2)
		req.Escrow, req.ShopRegion = e.escr.NostrPubHex(), "JP-27-27100"
		u.send(t, ctx, e.shopper.NostrPubHex(), oid2, proto.TypeOrderRequest, req)
		u.wait(t, ctx, oid2, proto.TypeOrderQuote, &q)
		if q.Accept || q.RejectReason != proto.RejectRegion {
			t.Fatalf("quote %+v", q)
		}
		// above the order limit
		oid3 := newOrderID()
		req = request(oid3)
		req.Escrow, req.Items = e.escr.NostrPubHex(), []proto.Item{{SKU: "A-200", Qty: 20}}
		u.send(t, ctx, e.shopper.NostrPubHex(), oid3, proto.TypeOrderRequest, req)
		u.wait(t, ctx, oid3, proto.TypeOrderQuote, &q)
		if q.Accept || q.RejectReason != proto.RejectLimit {
			t.Fatalf("quote %+v", q)
		}
		// a request whose key_proof is not the requester's (§4.4.1)
		oid4 := newOrderID()
		req = request(oid4)
		req.Escrow, req.KeyProof = e.escr.NostrPubHex(), request(oid3).KeyProof
		u.send(t, ctx, e.shopper.NostrPubHex(), oid4, proto.TypeOrderRequest, req)
		u.wait(t, ctx, oid4, proto.TypeOrderQuote, &q)
		if q.Accept || q.RejectReason != proto.RejectInvalid {
			t.Fatalf("quote %+v", q)
		}
	})
}
