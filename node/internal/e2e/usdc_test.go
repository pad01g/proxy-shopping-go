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
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/pad01g/proxy-shopping-go/node/internal/contract"
	"github.com/pad01g/proxy-shopping-go/node/internal/delivery"
	"github.com/pad01g/proxy-shopping-go/node/internal/evm"
	"github.com/pad01g/proxy-shopping-go/node/internal/node"
	"github.com/pad01g/proxy-shopping-go/node/internal/proto"
	"github.com/pad01g/proxy-shopping-go/node/internal/shopper"
	"github.com/pad01g/proxy-shopping-go/node/internal/testutil"
)

// TestUSDCOrder runs a USDC order through the nodes on anvil: the user deploys the predicted Safe, funds it and
// pays the escrow fee, the shopper verifies owners, module and balance, buys, and executes the user's release.
func TestUSDCOrder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	node.ProfileInterval = time.Minute
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
`, botURL), ""))
	e.escrowAdmin = runNode(t, ctx, nodeYAML("escrow", "escrow-1", freePort(t), freePort(t), e.relays, chainYAML, ca, t.TempDir(), `escrow:
  upfront_fee: {bps: 50, min_sats: "1000", min_usdc: "0.50"}
  dispute_fee_bps: 200
`, ""))
	e.seedTrust(t, ctx, coord, "usdc-evm")
	u := newUser(t, ctx, e.relays)
	testutil.Fund(t, ctx, client, d, u.keys.EVMAddress(), 1_000_000_000)

	oid := newOrderID()
	k, _ := delivery.NewKey()
	ct, _ := delivery.Seal(k, nil, oid, delivery.Address{Name: "A", PostalCode: "1", Address: "x", Phone: "0"})
	kShopper, _ := delivery.WrapKey(u.keys.NostrSecretHex(), e.shopper.NostrPubHex(), k)
	kEscrow, _ := delivery.WrapKey(u.keys.NostrSecretHex(), e.escr.NostrPubHex(), k)
	req := proto.OrderRequest{
		ShopURL: e.shopURL, ShopRegion: "JP-13-13104", Items: []proto.Item{{SKU: "A-100", Qty: 1}}, Payment: proto.AssetUSDC,
		Escrow: e.escr.NostrPubHex(), Operator: e.operator.NostrPubHex(), Coordinator: coordinatorPub,
		Delivery:       proto.Delivery{Ciphertext: ct, KeyForShopper: kShopper, KeyForEscrow: kEscrow},
		UserEVMAddress: u.keys.EVMAddress().Hex(), Relays: e.relays,
	}
	u.send(t, ctx, e.shopper.NostrPubHex(), oid, proto.TypeOrderRequest, req)
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
	u.send(t, ctx, e.shopper.NostrPubHex(), oid, proto.TypeOrderAccept, proto.OrderAccept{QuoteID: qm.Inner.ID})

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
	u.send(t, ctx, e.shopper.NostrPubHex(), oid, proto.TypeOrderFunded, proto.OrderFunded{
		Asset: proto.AssetUSDC, Safe: predicted.Hex(), DeployTx: deploy.TxHash.Hex(), FundTx: fund.TxHash.Hex(), FeeTx: fee.TxHash.Hex(), Amount: q.LockAmount,
	})
	u.wait(t, ctx, oid, proto.TypeOrderPurchased, nil)
	waitUntil(t, ctx, "delivered", func() bool { return e.shopperOrder(t, oid).ShipStatus == "delivered" })

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
}
