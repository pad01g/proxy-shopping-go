//go:build integration

package e2e

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/contract"
	"github.com/pad01g/proxy-shopping-go/node/internal/delivery"
	"github.com/pad01g/proxy-shopping-go/node/internal/escrow"
	"github.com/pad01g/proxy-shopping-go/node/internal/evm"
	"github.com/pad01g/proxy-shopping-go/node/internal/keys"
	"github.com/pad01g/proxy-shopping-go/node/internal/node"
	"github.com/pad01g/proxy-shopping-go/node/internal/proto"
	"github.com/pad01g/proxy-shopping-go/node/internal/shopper"
	"github.com/pad01g/proxy-shopping-go/node/internal/testutil"
)

// TestUSDCOrder runs a USDC order through the nodes on anvil: the user deploys the predicted Safe, funds it and
// pays the escrow fee, the shopper verifies owners, module and balance, buys, and executes the user's release.
func TestUSDCOrder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	node.ProfileInterval = time.Minute
	escrow.WatchInterval = time.Second
	coord := labKeys(t, "coordinator-1")
	coordinatorPub = coord.NostrPubHex()

	rpcURL := testutil.StartAnvil(t)
	client, err := evm.Dial(ctx, rpcURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := testutil.DeployLab(t, ctx, client)
	depFile := filepath.Join(t.TempDir(), "31337.json")
	data, _ := json.Marshal(d)
	if err := os.WriteFile(depFile, data, 0o600); err != nil {
		t.Fatal(err)
	}

	e := &env{relays: []string{testutil.StartRelay(t), testutil.StartRelay(t)}}
	e.shopper, e.escr, e.operator = labKeys(t, "shopper-1"), labKeys(t, "escrow-1"), labKeys(t, "operator-1")
	shop, ca := fakeShop(t)
	e.shopURL = shop.URL + "/"
	bot, botURL := startBot(t)
	e.bot = bot
	for _, a := range []common.Address{e.shopper.EVMAddress(), e.escr.EVMAddress()} {
		testutil.Fund(t, ctx, client, d, a, 0)
	}
	chainYAML := fmt.Sprintf("chain:\n  evm: {chain_id: 31337, rpc: %q, deployments: %q}\n", rpcURL, depFile)
	e.shopperAdmin = runNode(t, ctx, nodeYAML("shopper", "shopper-1", freePort(t), freePort(t), e.relays, chainYAML, ca, t.TempDir(), fmt.Sprintf(`shopper:
  bot_url: %q
  payments: [usdc-evm]
  currencies: [JPY, USD]
  fee: {bps: 500, min: {amount: "300", currency: JPY}}
  max_order: {amount: "200000", currency: JPY}
  risk: {allowlist: [127.0.0.1], known_gateways: [cardgw.test], threshold: 70}
  timelock: {btc_t1_blocks: 20, btc_t2_blocks: 30, evm_t1_seconds: 3600, evm_t2_seconds: 7200}
  tracking_poll_seconds: 1
  min_t1_remaining_seconds: 1800
  allow_private_shops: true
`, botURL), ""))
	e.escrowAdmin = runNode(t, ctx, nodeYAML("escrow", "escrow-1", freePort(t), freePort(t), e.relays, chainYAML, ca, t.TempDir(), `escrow:
  upfront_fee: {bps: 50, min_sats: "1000", min_usdc: "0.50"}
  dispute_fee_bps: 200
`, ""))
	e.seedTrust(t, ctx, coord, "usdc-evm")
	u := newUser(t, ctx, e.relays)
	testutil.Fund(t, ctx, client, d, u.keys.EVMAddress(), 1_000_000_000)

	// place places and funds a USDC order and waits for the purchase; it returns the signed agreement too
	type placed struct {
		id        string
		req       proto.OrderRequest
		q         proto.OrderQuote
		safe      common.Address
		lock      *big.Int
		events    []*nostr.Event
		keyEscrow string
	}
	place := func() placed {
		oid := newOrderID()
		k, _ := delivery.NewKey()
		ct, _ := delivery.Seal(k, nil, oid, delivery.Address{Name: "A", PostalCode: "1", Address: "x", Phone: "0"})
		kShopper, _ := delivery.WrapKey(u.keys.NostrSecretHex(), e.shopper.NostrPubHex(), k)
		kEscrow, _ := delivery.WrapKey(u.keys.NostrSecretHex(), e.escr.NostrPubHex(), k)
		proof, err := keys.SignKeyProofEVM(u.keys.EVM, oid, u.keys.NostrPubHex())
		if err != nil {
			t.Fatal(err)
		}
		req := proto.OrderRequest{
			ShopURL: e.shopURL, ShopRegion: "JP-13-13104", Items: []proto.Item{{SKU: "A-100", Qty: 1}}, Payment: proto.AssetUSDC,
			Escrow: e.escr.NostrPubHex(), Operator: e.operator.NostrPubHex(), Coordinator: coordinatorPub,
			Delivery:       proto.Delivery{Ciphertext: ct, KeyForShopper: kShopper, KeyForEscrowSHA256: proto.EscrowKeyHash(kEscrow)},
			KeyProof:       proof,
			UserEVMAddress: u.keys.EVMAddress().Hex(), Relays: e.relays,
		}
		reqEv := u.send(t, ctx, e.shopper.NostrPubHex(), oid, proto.TypeOrderRequest, req)
		u.send(t, ctx, e.shopper.NostrPubHex(), oid, proto.TypeOrderEscrowKey, proto.EscrowKey{KeyForEscrow: kEscrow})
		var q proto.OrderQuote
		qm := u.wait(t, ctx, oid, proto.TypeOrderQuote, &q)
		if !q.Accept {
			t.Fatalf("rejected: %s %s", q.RejectReason, q.Detail)
		}
		// 4300 JPY at 150 JPY/USDC = 28.666667 USDC
		if q.LockAmount != "28666667" || q.PayoutFeeReserve != "0" || q.EscrowUpfrontFee != "500000" {
			t.Fatalf("quote lock %s reserve %s fee %s", q.LockAmount, q.PayoutFeeReserve, q.EscrowUpfrontFee)
		}
		os, err := contract.SafeOf(d, &req, &q)
		if err != nil {
			t.Fatal(err)
		}
		predicted, err := client.PredictSafe(ctx, d, os, oid)
		if err != nil || predicted.Hex() != q.EscrowAddress {
			t.Fatalf("safe %s, recomputed %s (%v)", q.EscrowAddress, predicted.Hex(), err)
		}
		accEv := u.send(t, ctx, e.shopper.NostrPubHex(), oid, proto.TypeOrderAccept, proto.OrderAccept{QuoteID: qm.Inner.ID})

		deploy, err := client.DeploySafe(ctx, u.keys.EVM, d, os, oid)
		if err != nil {
			t.Fatal(err)
		}
		lock, _ := new(big.Int).SetString(q.LockAmount, 10)
		fund, err := client.TransferToken(ctx, u.keys.EVM, d.USDC, predicted, lock)
		if err != nil {
			t.Fatal(err)
		}
		feeAmt, _ := new(big.Int).SetString(q.EscrowUpfrontFee, 10)
		fee, err := client.TransferToken(ctx, u.keys.EVM, d.USDC, common.HexToAddress(q.EscrowEVMAddress), feeAmt)
		if err != nil {
			t.Fatal(err)
		}
		fundEv := u.send(t, ctx, e.shopper.NostrPubHex(), oid, proto.TypeOrderFunded, proto.OrderFunded{
			Asset: proto.AssetUSDC, Safe: predicted.Hex(), DeployTx: strings.ToLower(deploy.TxHash.Hex()), FundTx: strings.ToLower(fund.TxHash.Hex()),
			FeeTx: strings.ToLower(fee.TxHash.Hex()), Amount: q.LockAmount,
		})
		evs := []*nostr.Event{reqEv, qm.Inner, accEv, fundEv}
		u.send(t, ctx, e.escr.NostrPubHex(), oid, proto.TypeEscrowNotice, proto.EscrowNotice{Request: reqEv, Quote: qm.Inner, Accept: accEv, Funded: fundEv})
		u.wait(t, ctx, oid, proto.TypeOrderPurchased, nil)
		waitUntil(t, ctx, "delivered", func() bool { return e.shopperOrder(t, oid).ShipStatus == "delivered" })
		return placed{id: oid, req: req, q: q, safe: predicted, lock: lock, events: evs, keyEscrow: kEscrow}
	}

	o := place()
	oid, q, predicted, lock := o.id, o.q, o.safe, o.lock
	rel, err := evm.ReleaseTx(d.USDC, common.HexToAddress(q.ShopperEVMAddress), lock, big.NewInt(0))
	if err != nil {
		t.Fatal(err)
	}
	sig, _ := evm.Sign(rel.Hash(d.ChainID, predicted), u.keys.EVM)
	before, _ := client.BalanceOf(ctx, d.USDC, e.shopper.EVMAddress())
	u.send(t, ctx, e.shopper.NostrPubHex(), oid, proto.TypeOrderRelease, proto.Release{Asset: proto.AssetUSDC, SafeTx: &rel, Signature: "0x" + hex.EncodeToString(sig)})
	var done proto.TxRef
	u.wait(t, ctx, oid, proto.TypeOrderCompleted, &done)
	after, _ := client.BalanceOf(ctx, d.USDC, e.shopper.EVMAddress())
	if new(big.Int).Sub(after, before).Cmp(lock) != 0 {
		t.Fatalf("shopper received %s", new(big.Int).Sub(after, before))
	}
	if st := e.shopperOrder(t, oid).State; st != shopper.StateCompleted {
		t.Fatalf("state %s", st)
	}

	// a disputed order: the escrow divides the balance when it rules; one unit sent to the Safe before the
	// ruling is carried out stays behind, and both sides still see the order settled (§4.8)
	o2 := place()
	open := proto.DisputeOpen{Claim: proto.ClaimWrongItem, Text: "wrong beans", Evidence: proto.DisputeEvidence{Messages: o2.events, DeliveryKeyForEscrow: o2.keyEscrow}}
	u.send(t, ctx, e.escr.NostrPubHex(), o2.id, proto.TypeDisputeOpen, open)
	u.send(t, ctx, e.shopper.NostrPubHex(), o2.id, proto.TypeDisputeOpen, open)
	var c escrow.Case
	waitUntil(t, ctx, "case open", func() bool {
		admin(t, "GET", e.escrowAdmin+"/cases/"+o2.id, nil, &c)
		return c.State == escrow.CaseOpen
	})
	waitUntil(t, ctx, "shopper disputed", func() bool { return e.shopperOrder(t, o2.id).Dispute != nil })
	// the shopper is away while the ruling is signed and the dust arrives
	if code := admin(t, "POST", e.shopperAdmin+"/admin/pause", map[string]int{"seconds": 120}, nil); code != 200 {
		t.Fatalf("pause: %d", code)
	}
	fee2 := new(big.Int).Div(new(big.Int).Mul(o2.lock, big.NewInt(200)), big.NewInt(10000))
	userShare := big.NewInt(10_000_000)
	shopperShare := new(big.Int).Sub(new(big.Int).Sub(o2.lock, fee2), userShare)
	var ruling proto.Ruling
	if code := admin(t, "POST", e.escrowAdmin+"/cases/"+o2.id+"/rule", map[string]string{"user": userShare.String(), "shopper": shopperShare.String()}, &ruling); code != 200 {
		t.Fatalf("rule: %d", code)
	}
	if _, err := client.TransferToken(ctx, u.keys.EVM, d.USDC, o2.safe, big.NewInt(1)); err != nil {
		t.Fatal(err)
	}
	if code := admin(t, "POST", e.shopperAdmin+"/admin/resume", nil, nil); code != 200 {
		t.Fatalf("resume: %d", code)
	}
	var cs proto.TxRef
	u.wait(t, ctx, o2.id, proto.TypeDisputeCountersigned, &cs)
	if bal, _ := client.BalanceOf(ctx, d.USDC, o2.safe); bal.Int64() != 1 {
		t.Fatalf("safe keeps %s", bal)
	}
	if got := e.shopperOrder(t, o2.id); got.State != shopper.StateSettled || got.PayoutBy != "ruling" {
		t.Fatalf("shopper %s %s", got.State, got.Error)
	}
	waitUntil(t, ctx, "case closed with dust left", func() bool {
		admin(t, "GET", e.escrowAdmin+"/cases/"+o2.id, nil, &c)
		return c.State == escrow.CaseClosed
	})
	if !strings.EqualFold(c.PayoutTx, cs.TxID) {
		t.Fatalf("case closed by %s, countersigned %s", c.PayoutTx, cs.TxID)
	}
}
