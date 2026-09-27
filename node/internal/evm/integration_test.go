//go:build integration

package evm_test

import (
	"context"
	"math/big"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/pad01g/proxy-shopping-go/node/internal/contract"
	"github.com/pad01g/proxy-shopping-go/node/internal/evm"
	"github.com/pad01g/proxy-shopping-go/node/internal/keys"
	"github.com/pad01g/proxy-shopping-go/node/internal/proto"
	"github.com/pad01g/proxy-shopping-go/node/internal/testutil"
)

func labKey(t *testing.T, name string) *keys.Set {
	t.Helper()
	s, err := keys.LoadMnemonicFile(filepath.Join("..", "..", "..", "lab", "keys", name+".mnemonic"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestSafeFlowsOnAnvil runs the three USDC paths against the real contracts: 2-of-3 release, a MultiSend
// split signed by the escrow, and the module's timelock branches.
func TestSafeFlowsOnAnvil(t *testing.T) {
	url := testutil.StartAnvil(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c, err := evm.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := testutil.DeployLab(t, ctx, c)
	user, shopper, escrow := labKey(t, "user-1"), labKey(t, "shopper-1"), labKey(t, "escrow-1")
	for _, s := range []*keys.Set{user, shopper, escrow} {
		testutil.Fund(t, ctx, c, d, s.EVMAddress(), 0)
	}
	testutil.Fund(t, ctx, c, d, user.EVMAddress(), 1_000_000_000)

	now, err := c.LatestTime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	newSafe := func(orderID string, lock int64) (evm.OrderSafe, common.Address) {
		t.Helper()
		os := evm.NewOrderSafe(d, user.EVMAddress(), shopper.EVMAddress(), escrow.EVMAddress(), now+3600, now+7200)
		predicted, err := c.PredictSafe(ctx, d, os, orderID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.DeploySafe(ctx, user.EVM, d, os, orderID); err != nil {
			t.Fatal(err)
		}
		code, err := c.Eth.CodeAt(ctx, predicted, nil)
		if err != nil || len(code) == 0 {
			t.Fatalf("no Safe at the predicted address %s", predicted.Hex())
		}
		if _, err := c.TransferToken(ctx, user.EVM, d.USDC, predicted, big.NewInt(lock)); err != nil {
			t.Fatal(err)
		}
		st, err := c.InspectSafe(ctx, predicted, d.Module)
		if err != nil {
			t.Fatal(err)
		}
		if st.Threshold.Int64() != 2 || len(st.Owners) != 3 || !st.ModuleEnabled || st.Config.Shopper != shopper.EVMAddress() || st.Config.T1 != os.T1 {
			t.Fatalf("safe state %+v", st)
		}
		return os, predicted
	}
	balance := func(a common.Address) *big.Int {
		b, err := c.BalanceOf(ctx, d.USDC, a)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	// 1. release: the user signs, the shopper checks, countersigns and executes
	_, safe := newSafe("000102030405060708090a0b0c0d0e0f", 45_000_000)
	nonce, _ := c.SafeNonce(ctx, safe)
	rel, err := evm.ReleaseTx(d.USDC, shopper.EVMAddress(), big.NewInt(45_000_000), nonce)
	if err != nil {
		t.Fatal(err)
	}
	h := rel.Hash(d.ChainID, safe)
	onChain, err := c.Call(ctx, safe, evm.SafeABI, "getTransactionHash", rel.To, rel.Value, rel.Data, rel.Operation,
		big.NewInt(0), big.NewInt(0), big.NewInt(0), common.Address{}, common.Address{}, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if common.Hash(onChain[0].([32]byte)) != h {
		t.Fatalf("EIP-712 hash %s differs from Safe.getTransactionHash %x", h.Hex(), onChain[0])
	}
	userSig, _ := evm.Sign(h, user.EVM)
	want := map[common.Address]*big.Int{shopper.EVMAddress(): big.NewInt(45_000_000)}
	if err := contract.CheckSafePayout(rel, d, d.ChainID, safe, nonce, want, userSig, user.EVMAddress()); err != nil {
		t.Fatal(err)
	}
	shopperSig, _ := evm.Sign(h, shopper.EVM)
	if _, err := c.ExecTransaction(ctx, shopper.EVM, safe, rel, map[common.Address][]byte{user.EVMAddress(): userSig, shopper.EVMAddress(): shopperSig}); err != nil {
		t.Fatal(err)
	}
	if got := balance(shopper.EVMAddress()); got.Int64() != 45_000_000 {
		t.Fatalf("shopper got %s", got)
	}

	// 2. ruling: MultiSend split signed by the escrow, countersigned and executed by the user
	_, safe2 := newSafe("101112131415161718191a1b1c1d1e1f", 50_000_000)
	nonce, _ = c.SafeNonce(ctx, safe2)
	split, err := evm.SplitTx(d.Safe.MultiSendCallOnly, d.USDC, []evm.Transfer{
		{To: user.EVMAddress(), Amount: big.NewInt(30_000_000)},
		{To: shopper.EVMAddress(), Amount: big.NewInt(19_000_000)},
		{To: escrow.EVMAddress(), Amount: big.NewInt(1_000_000)},
	}, nonce)
	if err != nil {
		t.Fatal(err)
	}
	h2 := split.Hash(d.ChainID, safe2)
	escrowSig, _ := evm.Sign(h2, escrow.EVM)
	userSig2, _ := evm.Sign(h2, user.EVM)
	before := balance(user.EVMAddress())
	if _, err := c.ExecTransaction(ctx, user.EVM, safe2, split, map[common.Address][]byte{escrow.EVMAddress(): escrowSig, user.EVMAddress(): userSig2}); err != nil {
		t.Fatal(err)
	}
	if got := new(big.Int).Sub(balance(user.EVMAddress()), before); got.Int64() != 30_000_000 {
		t.Fatalf("user got %s from the split", got)
	}
	if got := balance(escrow.EVMAddress()); got.Int64() != 1_000_000 {
		t.Fatalf("escrow got %s", got)
	}
	if got := balance(safe2); got.Sign() != 0 {
		t.Fatalf("safe keeps %s", got)
	}

	// 3. timelocks: the shopper claims after t1; before t1 the module refuses
	_, safe3 := newSafe("202122232425262728292a2b2c2d2e2f", 10_000_000)
	if _, err := c.ClaimByShopper(ctx, shopper.EVM, d.Module, safe3); err == nil {
		t.Fatal("claim before t1 succeeded")
	}
	if _, err := c.RefundToUser(ctx, user.EVM, d.Module, safe3); err == nil {
		t.Fatal("refund before t2 succeeded")
	}
	testutil.AdvanceTime(t, ctx, c, 3601)
	shopperBefore := balance(shopper.EVMAddress())
	if _, err := c.ClaimByShopper(ctx, shopper.EVM, d.Module, safe3); err != nil {
		t.Fatalf("claim after t1: %v", err)
	}
	if got := new(big.Int).Sub(balance(shopper.EVMAddress()), shopperBefore); got.Int64() != 10_000_000 {
		t.Fatalf("claim paid %s", got)
	}

	// 4. the user takes the money back after t2
	_, safe4 := newSafe("303132333435363738393a3b3c3d3e3f", 7_000_000)
	testutil.AdvanceTime(t, ctx, c, 7201)
	if _, err := c.RefundToUser(ctx, shopper.EVM, d.Module, safe4); err == nil {
		t.Fatal("shopper could use refundToUser")
	}
	if _, err := c.RefundToUser(ctx, user.EVM, d.Module, safe4); err != nil {
		t.Fatalf("refund after t2: %v", err)
	}
	if got := balance(safe4); got.Sign() != 0 {
		t.Fatalf("safe keeps %s after refund", got)
	}
}

// TestFundingSettlementAndReplacementOnAnvil: the fund_tx of a USDC order must itself carry the lock; a Safe
// paid out with dust left in it counts as settled by the transfer out; a stuck transaction is replaced with
// the same nonce (second review).
func TestFundingSettlementAndReplacementOnAnvil(t *testing.T) {
	url := testutil.StartAnvil(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c, err := evm.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := testutil.DeployLab(t, ctx, c)
	user, shopper, escrow := labKey(t, "user-1"), labKey(t, "shopper-1"), labKey(t, "escrow-1")
	for _, s := range []*keys.Set{user, shopper, escrow} {
		testutil.Fund(t, ctx, c, d, s.EVMAddress(), 0)
	}
	testutil.Fund(t, ctx, c, d, user.EVMAddress(), 1_000_000_000)
	now, _ := c.LatestTime(ctx)
	const orderID = "404142434445464748494a4b4c4d4e4f"
	const lock = 20_000_000
	os := evm.NewOrderSafe(d, user.EVMAddress(), shopper.EVMAddress(), escrow.EVMAddress(), now+3600, now+7200)
	safe, err := c.PredictSafe(ctx, d, os, orderID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeploySafe(ctx, user.EVM, d, os, orderID); err != nil {
		t.Fatal(err)
	}
	early, err := c.TransferToken(ctx, user.EVM, d.USDC, safe, big.NewInt(1))
	if err != nil {
		t.Fatal(err)
	}
	full, err := c.TransferToken(ctx, user.EVM, d.USDC, safe, big.NewInt(lock))
	if err != nil {
		t.Fatal(err)
	}
	req := &proto.OrderRequest{Payment: proto.AssetUSDC, UserEVMAddress: user.EVMAddress().Hex()}
	q := &proto.OrderQuote{Accept: true, Asset: proto.AssetUSDC, LockAmount: big.NewInt(lock).String(), EscrowUpfrontFee: "0",
		Timelock: &proto.Timelock{T1: int64(now + 3600), T2: int64(now + 7200)}, ShopperEVMAddress: shopper.EVMAddress().Hex(),
		EscrowEVMAddress: escrow.EVMAddress().Hex(), EscrowAddress: safe.Hex()}
	f := &proto.OrderFunded{Asset: proto.AssetUSDC, Safe: safe.Hex(), FundTx: early.TxHash.Hex()}
	// a tiny early transfer named as the funding (the rest came later) does not date the funding
	if _, err := contract.VerifySafeFunding(ctx, c, d, os, orderID, req, q, f, 1); !contract.IsDefinite(err) {
		t.Fatalf("small fund_tx accepted: %v", err)
	}
	f.FundTx = full.TxHash.Hex()
	if _, err := contract.VerifySafeFunding(ctx, c, d, os, orderID, req, q, f, 1); err != nil {
		t.Fatalf("fund_tx with the lock: %v", err)
	}

	// not settled while the lock is there
	if by, err := contract.SafeSettlement(ctx, c, d, safe, big.NewInt(lock), ""); err != nil || by != "" {
		t.Fatalf("settled early: %s %v", by, err)
	}
	// a ruling pays out the balance at signing (lock + 1); one more unit arrives before it is carried out
	nonce, _ := c.SafeNonce(ctx, safe)
	split, _ := evm.SplitTx(d.Safe.MultiSendCallOnly, d.USDC, []evm.Transfer{
		{To: user.EVMAddress(), Amount: big.NewInt(lock/2 + 1)}, {To: shopper.EVMAddress(), Amount: big.NewInt(lock / 2)}}, nonce)
	h := split.Hash(d.ChainID, safe)
	es, _ := evm.Sign(h, escrow.EVM)
	us, _ := evm.Sign(h, user.EVM)
	testutil.Fund(t, ctx, c, d, escrow.EVMAddress(), 1)
	if _, err := c.TransferToken(ctx, escrow.EVM, d.USDC, safe, big.NewInt(1)); err != nil {
		t.Fatal(err)
	}
	r, err := c.ExecTransaction(ctx, user.EVM, safe, split, map[common.Address][]byte{escrow.EVMAddress(): es, user.EVMAddress(): us})
	if err != nil {
		t.Fatal(err)
	}
	if bal, _ := c.BalanceOf(ctx, d.USDC, safe); bal.Int64() != 1 {
		t.Fatalf("safe keeps %s", bal)
	}
	// settled: found from the logs, and from a named transaction
	want := strings.ToLower(r.TxHash.Hex())
	if by, err := contract.SafeSettlement(ctx, c, d, safe, big.NewInt(lock), ""); err != nil || by != want {
		t.Fatalf("settlement from the logs: %s %v", by, err)
	}
	if by, err := contract.SafeSettlement(ctx, c, d, safe, big.NewInt(lock), r.TxHash.Hex()); err != nil || by != want {
		t.Fatalf("settlement from the named tx: %s %v", by, err)
	}

	// a transaction that is not mined is replaced: same nonce, higher fee
	if err := c.RPC.CallContext(ctx, nil, "evm_setAutomine", false); err != nil {
		t.Fatal(err)
	}
	data, _ := evm.ERC20ABI.Pack("transfer", shopper.EVMAddress(), big.NewInt(5))
	first, err := c.Send(ctx, user.EVM, d.USDC, data, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.SendReplacing(ctx, user.EVM, d.USDC, data, nil, first)
	if err != nil {
		t.Fatalf("replacement refused: %v", err)
	}
	t1, _, err1 := c.Eth.TransactionByHash(ctx, first)
	t2, _, err2 := c.Eth.TransactionByHash(ctx, second)
	if err2 != nil || (err1 == nil && t1.Nonce() != t2.Nonce()) || second == first {
		t.Fatalf("replacement: %v %v", err1, err2)
	}
	if err := c.RPC.CallContext(ctx, nil, "evm_mine"); err != nil {
		t.Fatal(err)
	}
	_ = c.RPC.CallContext(ctx, nil, "evm_setAutomine", true)
	if rr, err := c.Receipt(ctx, second); err != nil || rr.Status != 1 {
		t.Fatalf("replacement not mined: %v", err)
	}
	if _, err := c.Receipt(ctx, first); err == nil {
		t.Fatal("both transactions mined")
	}
}
