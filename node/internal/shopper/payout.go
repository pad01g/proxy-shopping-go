package shopper

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/ethereum/go-ethereum/common"

	"github.com/pad01g/proxy-shopping-go/node/internal/amount"
	"github.com/pad01g/proxy-shopping-go/node/internal/btc"
	"github.com/pad01g/proxy-shopping-go/node/internal/contract"
	"github.com/pad01g/proxy-shopping-go/node/internal/evm"
	"github.com/pad01g/proxy-shopping-go/node/internal/messenger"
	"github.com/pad01g/proxy-shopping-go/node/internal/proto"
	"github.com/pad01g/proxy-shopping-go/node/internal/trust"
)

func parseRat(s string) (*big.Rat, error) { return amount.Parse(s) }

func formatCur(r *big.Rat, cur string) string { return amount.FormatCurrency(r, cur) }

// funded tells whether the escrow of the order exists and is not paid out yet.
func (o *Order) funded() bool {
	return (o.Outpoint != nil || o.Safe != "") && o.PayoutTx == "" && !terminal(o.State)
}

func (o *Order) reserve() int64 {
	n, _ := amount.ParseInt(o.Quote.PayoutFeeReserve)
	if n == nil {
		return 0
	}
	return n.Int64()
}

func (o *Order) lock() *big.Int {
	n, _ := amount.ParseInt(o.Quote.LockAmount)
	if n == nil {
		return new(big.Int)
	}
	return n
}

// donation returns the donation address and bps of the operator's list for the order (§2.3), if any.
func (e *Engine) donation(o *Order) (btcAddr, evmAddr string, bps int64) {
	ev := e.Trust.Get(trust.Key{Kind: trust.KindList, PubKey: o.Request.Operator, D: e.Network})
	if ev == nil {
		return "", "", 0
	}
	l, err := trust.ParseList(ev)
	if err != nil || len(l.Donation) == 0 {
		return "", "", 0
	}
	var d struct {
		BTCAddress string `json:"btc_address"`
		EVMAddress string `json:"evm_address"`
		BPS        int64  `json:"bps"`
	}
	if json.Unmarshal(l.Donation, &d) != nil || d.BPS <= 0 {
		return "", "", 0
	}
	return d.BTCAddress, d.EVMAddress, d.BPS
}

func (e *Engine) onRelease(ctx context.Context, msg *messenger.Message) {
	var r proto.Release
	if err := msg.Decode(&r); err != nil {
		return
	}
	defer e.lock(msg.OrderID)()
	o, ok, err := e.Order(msg.OrderID)
	if err != nil || !ok || o.User != msg.From {
		return
	}
	if !o.funded() {
		e.log.Warn("release for an order without open escrow", "order", o.ID, "state", o.State)
		return
	}
	txid, err := e.countersignRelease(ctx, o, &r)
	if err != nil {
		e.fail(o.ID, "release rejected", err)
		_, _ = e.send(ctx, o, o.User, proto.TypeChat, proto.Chat{Text: "release rejected: " + err.Error()})
		return
	}
	o, _ = e.update(o.ID, func(o *Order) error {
		o.Events["release"] = msg.Inner
		o.PayoutTx, o.PayoutBy = txid, "release"
		o.set(StateCompleted, txid)
		return nil
	})
	if _, err := e.send(ctx, o, o.User, proto.TypeOrderCompleted, proto.TxRef{TxID: txid}); err != nil {
		e.fail(o.ID, "completed not sent", err)
	}
	e.log.Info("released", "order", o.ID, "tx", txid)
}

func (e *Engine) countersignRelease(ctx context.Context, o *Order, r *proto.Release) (string, error) {
	switch o.Quote.Asset {
	case proto.AssetBTC:
		if r.PSBT == "" {
			return "", errors.New("release without psbt")
		}
		esc, err := contract.BTCEscrow(&o.Request, o.Quote)
		if err != nil {
			return "", err
		}
		p, err := btc.DecodePSBT(r.PSBT)
		if err != nil {
			return "", err
		}
		pay := o.lock().Int64() - o.reserve()
		extra := map[string]int64{}
		want := map[string]int64{o.Quote.ShopperBTCAddress: pay}
		if addr, _, bps := e.donation(o); addr != "" {
			max := amount.BPS(big.NewInt(pay), bps).Int64()
			extra[addr] = max
			want[o.Quote.ShopperBTCAddress] = pay - max
		}
		if err := contract.CheckBTCPayout(p, esc, *o.Outpoint, want, extra, o.reserve(), esc.User); err != nil {
			return "", err
		}
		return e.signAndBroadcast(ctx, o, esc, p)
	case proto.AssetUSDC:
		if r.SafeTx == nil {
			return "", errors.New("release without safe_tx")
		}
		d, err := e.Deployments()
		if err != nil {
			return "", err
		}
		want := map[common.Address]*big.Int{common.HexToAddress(o.Quote.ShopperEVMAddress): o.lock()}
		return e.execSafe(ctx, o, d, *r.SafeTx, r.Signature, want, common.HexToAddress(o.Request.UserEVMAddress))
	}
	return "", fmt.Errorf("unknown asset %q", o.Quote.Asset)
}

// signAndBroadcast adds our signature to a 2-of-3 PSBT and broadcasts it.
func (e *Engine) signAndBroadcast(ctx context.Context, o *Order, esc btc.Escrow, p *psbt.Packet) (string, error) {
	key, err := e.Keys.OrderKey(o.ID)
	if err != nil {
		return "", err
	}
	if err := btc.Sign(p, key); err != nil {
		return "", err
	}
	tx, err := esc.Finalize(p, btc.PathMultisig)
	if err != nil {
		return "", err
	}
	txid, err := e.BTC.Broadcast(ctx, btc.TxHex(tx))
	if err != nil {
		return "", fmt.Errorf("broadcast: %w", err)
	}
	return txid, nil
}

// execSafe checks a SafeTx signed by the counterparty, signs it and executes it (we pay the gas).
func (e *Engine) execSafe(ctx context.Context, o *Order, d *evm.Deployments, t evm.SafeTx, sigHex string, want map[common.Address]*big.Int, signer common.Address) (string, error) {
	sig, err := contract.ParseSig(sigHex)
	if err != nil {
		return "", err
	}
	safe := common.HexToAddress(o.Safe)
	nonce, err := e.EVM.SafeNonce(ctx, safe)
	if err != nil {
		return "", err
	}
	if err := contract.CheckSafePayout(t, d, d.ChainID, safe, nonce, want, sig, signer); err != nil {
		return "", err
	}
	mine, err := evm.Sign(t.Hash(d.ChainID, safe), e.Keys.EVM)
	if err != nil {
		return "", err
	}
	rcpt, err := e.EVM.ExecTransaction(ctx, e.Keys.EVM, safe, t, map[common.Address][]byte{signer: sig, e.Keys.EVMAddress(): mine})
	if err != nil {
		return "", fmt.Errorf("execTransaction: %w", err)
	}
	return rcpt.TxHash.Hex(), nil
}

// offerRefund sends a cooperative refund (order.refund) signed by us after a failed purchase.
func (e *Engine) offerRefund(ctx context.Context, id string) error {
	o, ok, err := e.Order(id)
	if err != nil || !ok || !o.funded() {
		return err
	}
	body := proto.Release{Asset: o.Quote.Asset}
	switch o.Quote.Asset {
	case proto.AssetBTC:
		esc, err := contract.BTCEscrow(&o.Request, o.Quote)
		if err != nil {
			return err
		}
		p, err := esc.NewSpend(*o.Outpoint, []btc.Output{{Address: o.Request.UserBTCAddress, Amount: o.Outpoint.Amount - o.reserve()}}, btc.PathMultisig)
		if err != nil {
			return err
		}
		key, err := e.Keys.OrderKey(o.ID)
		if err != nil {
			return err
		}
		if err := btc.Sign(p, key); err != nil {
			return err
		}
		if body.PSBT, err = btc.EncodePSBT(p); err != nil {
			return err
		}
	case proto.AssetUSDC:
		d, err := e.Deployments()
		if err != nil {
			return err
		}
		safe := common.HexToAddress(o.Safe)
		nonce, err := e.EVM.SafeNonce(ctx, safe)
		if err != nil {
			return err
		}
		bal, err := e.EVM.BalanceOf(ctx, d.USDC, safe)
		if err != nil {
			return err
		}
		t, err := evm.ReleaseTx(d.USDC, common.HexToAddress(o.Request.UserEVMAddress), bal, nonce)
		if err != nil {
			return err
		}
		sig, err := evm.Sign(t.Hash(d.ChainID, safe), e.Keys.EVM)
		if err != nil {
			return err
		}
		body.SafeTx, body.Signature = &t, "0x"+hex.EncodeToString(sig)
	}
	_, err = e.send(ctx, o, o.User, proto.TypeOrderRefund, body)
	return err
}

func (e *Engine) onRuling(ctx context.Context, msg *messenger.Message) {
	var r proto.Ruling
	if err := msg.Decode(&r); err != nil {
		return
	}
	defer e.lock(msg.OrderID)()
	o, ok, err := e.Order(msg.OrderID)
	if err != nil || !ok || msg.From != o.Request.Escrow {
		return
	}
	o, _ = e.update(o.ID, func(o *Order) error {
		o.Ruling = &r
		o.Events["ruling"] = msg.Inner
		o.note(fmt.Sprintf("ruling user=%s shopper=%s fee=%s: %s", r.Split.User, r.Split.Shopper, r.Split.EscrowFee, r.Reason))
		return nil
	})
	if !o.funded() {
		return
	}
	shopperShare, err := amount.ParseInt(r.Split.Shopper)
	if err != nil {
		e.fail(o.ID, "ruling", err)
		return
	}
	userShare, _ := amount.ParseInt(r.Split.User)
	if shopperShare.Sign() == 0 {
		return // nothing for us; the user countersigns
	}
	if e.cfg.AcceptRulings == "favorable" && userShare != nil && shopperShare.Cmp(userShare) < 0 {
		e.log.Info("ruling not countersigned (policy favorable)", "order", o.ID)
		return
	}
	txid, err := e.countersignRuling(ctx, o, &r)
	if err != nil {
		e.fail(o.ID, "ruling not countersigned", err)
		return
	}
	o, _ = e.update(o.ID, func(o *Order) error {
		o.PayoutTx, o.PayoutBy = txid, "ruling"
		o.set(StateSettled, txid)
		return nil
	})
	for _, to := range []string{o.User, o.Request.Escrow} {
		if _, err := e.send(ctx, o, to, proto.TypeDisputeCountersigned, proto.TxRef{TxID: txid}); err != nil {
			e.fail(o.ID, "countersigned not sent", err)
		}
	}
	e.log.Info("ruling countersigned", "order", o.ID, "tx", txid)
}

func (e *Engine) countersignRuling(ctx context.Context, o *Order, r *proto.Ruling) (string, error) {
	parts := map[string]*big.Int{}
	for name, s := range map[string]string{"user": r.Split.User, "shopper": r.Split.Shopper, "escrow_fee": r.Split.EscrowFee} {
		v, err := amount.ParseInt(s)
		if err != nil {
			return "", fmt.Errorf("split.%s: %w", name, err)
		}
		parts[name] = v
	}
	sum := new(big.Int).Add(parts["user"], parts["shopper"])
	sum.Add(sum, parts["escrow_fee"])
	switch o.Quote.Asset {
	case proto.AssetBTC:
		if r.PSBT == "" {
			return "", errors.New("ruling without psbt")
		}
		if want := o.Outpoint.Amount - o.reserve(); sum.Int64() != want {
			return "", fmt.Errorf("split sums to %s, escrow holds %d after the reserve", sum, want)
		}
		esc, err := contract.BTCEscrow(&o.Request, o.Quote)
		if err != nil {
			return "", err
		}
		p, err := btc.DecodePSBT(r.PSBT)
		if err != nil {
			return "", err
		}
		want := map[string]int64{}
		add := func(addr string, v *big.Int) {
			if v.Sign() > 0 {
				want[addr] += v.Int64()
			}
		}
		add(o.Request.UserBTCAddress, parts["user"])
		add(o.Quote.ShopperBTCAddress, parts["shopper"])
		add(o.Quote.EscrowBTCFeeAddress, parts["escrow_fee"])
		if err := contract.CheckBTCPayout(p, esc, *o.Outpoint, want, nil, o.reserve(), esc.Escrow); err != nil {
			return "", err
		}
		return e.signAndBroadcast(ctx, o, esc, p)
	case proto.AssetUSDC:
		if r.SafeTx == nil {
			return "", errors.New("ruling without safe_tx")
		}
		d, err := e.Deployments()
		if err != nil {
			return "", err
		}
		bal, err := e.EVM.BalanceOf(ctx, d.USDC, common.HexToAddress(o.Safe))
		if err != nil {
			return "", err
		}
		if sum.Cmp(bal) != 0 {
			return "", fmt.Errorf("split sums to %s, safe holds %s", sum, bal)
		}
		want := map[common.Address]*big.Int{}
		add := func(a string, v *big.Int) {
			addr := common.HexToAddress(a)
			if want[addr] == nil {
				want[addr] = new(big.Int)
			}
			want[addr].Add(want[addr], v)
		}
		add(o.Request.UserEVMAddress, parts["user"])
		add(o.Quote.ShopperEVMAddress, parts["shopper"])
		add(o.Quote.EscrowEVMAddress, parts["escrow_fee"])
		return e.execSafe(ctx, o, d, *r.SafeTx, r.Signature, want, common.HexToAddress(o.Quote.EscrowEVMAddress))
	}
	return "", fmt.Errorf("unknown asset %q", o.Quote.Asset)
}

func (e *Engine) onCountersigned(ctx context.Context, msg *messenger.Message) {
	var ref proto.TxRef
	if msg.Decode(&ref) != nil {
		return
	}
	_, _ = e.update(msg.OrderID, func(o *Order) error {
		if msg.From != o.User || o.PayoutTx != "" {
			return errSkip
		}
		o.PayoutTx, o.PayoutBy = ref.TxID, "ruling"
		o.set(StateSettled, "countersigned by the user: "+ref.TxID)
		return nil
	})
}

// watchEscrows notices escrows spent by others and claims delivered orders after T1.
func (e *Engine) watchEscrows(ctx context.Context) {
	orders, err := e.Orders()
	if err != nil {
		return
	}
	for i := range orders {
		o := &orders[i]
		if !o.funded() {
			continue
		}
		wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		switch o.Quote.Asset {
		case proto.AssetBTC:
			e.watchBTC(wctx, o)
		case proto.AssetUSDC:
			e.watchSafe(wctx, o)
		}
		cancel()
	}
}

// claimable: a delivered order whose user neither released nor had a ruling carried out by T1.
func (o *Order) claimable() bool {
	return o.ShipStatus == "delivered" && o.PayoutTx == ""
}

func (e *Engine) watchBTC(ctx context.Context, o *Order) {
	spent, err := e.BTC.Outspend(ctx, o.Outpoint.TxID, o.Outpoint.Vout)
	if err != nil {
		return
	}
	if spent.Spent {
		e.closed(o.ID, spent.TxID)
		return
	}
	if !o.claimable() {
		return
	}
	tip, err := e.BTC.TipHeight(ctx)
	if err != nil || tip < o.Quote.Timelock.T1 {
		return
	}
	defer e.lock(o.ID)()
	if o, _, err = e.Order(o.ID); err != nil || !o.funded() || !o.claimable() {
		return
	}
	txid, err := e.claimBTC(ctx, o)
	if err != nil {
		e.fail(o.ID, "timelock claim failed", err)
		return
	}
	e.claimed(ctx, o.ID, txid)
}

func (e *Engine) claimBTC(ctx context.Context, o *Order) (string, error) {
	esc, err := contract.BTCEscrow(&o.Request, o.Quote)
	if err != nil {
		return "", err
	}
	p, err := esc.NewSpend(*o.Outpoint, []btc.Output{{Address: o.Quote.ShopperBTCAddress, Amount: o.Outpoint.Amount - o.reserve()}}, btc.PathShopperT1)
	if err != nil {
		return "", err
	}
	key, err := e.Keys.OrderKey(o.ID)
	if err != nil {
		return "", err
	}
	if err := btc.Sign(p, key); err != nil {
		return "", err
	}
	tx, err := esc.Finalize(p, btc.PathShopperT1)
	if err != nil {
		return "", err
	}
	return e.BTC.Broadcast(ctx, btc.TxHex(tx))
}

func (e *Engine) watchSafe(ctx context.Context, o *Order) {
	d, err := e.Deployments()
	if err != nil {
		return
	}
	safe := common.HexToAddress(o.Safe)
	bal, err := e.EVM.BalanceOf(ctx, d.USDC, safe)
	if err != nil {
		return
	}
	if bal.Sign() == 0 {
		e.closed(o.ID, "")
		return
	}
	if !o.claimable() {
		return
	}
	now, err := e.EVM.LatestTime(ctx)
	if err != nil || int64(now) < o.Quote.Timelock.T1 {
		return
	}
	defer e.lock(o.ID)()
	if o, _, err = e.Order(o.ID); err != nil || !o.funded() || !o.claimable() {
		return
	}
	rcpt, err := e.EVM.ClaimByShopper(ctx, e.Keys.EVM, d.Module, safe)
	if err != nil {
		e.fail(o.ID, "timelock claim failed", err)
		return
	}
	e.claimed(ctx, o.ID, rcpt.TxHash.Hex())
}

func (e *Engine) claimed(ctx context.Context, id, txid string) {
	o, err := e.update(id, func(o *Order) error {
		if o.PayoutTx != "" {
			return errSkip
		}
		o.PayoutTx, o.PayoutBy = txid, "timelock"
		o.set(StateClaimed, txid)
		return nil
	})
	if err != nil {
		return
	}
	e.log.Info("claimed after T1", "order", id, "tx", txid)
	_, _ = e.send(ctx, o, o.User, proto.TypeOrderCompleted, proto.TxRef{TxID: txid})
}

// closed records that somebody else spent the escrow (a ruling or refund countersigned by the user, or T2).
func (e *Engine) closed(id, txid string) {
	defer e.lock(id)()
	_, _ = e.update(id, func(o *Order) error {
		if o.PayoutTx != "" || terminal(o.State) {
			return errSkip
		}
		o.PayoutTx, o.PayoutBy = txid, "other"
		if txid == "" {
			o.PayoutTx = "(safe emptied)"
		}
		o.set(StateClosed, strings.TrimSpace("escrow spent "+txid))
		return nil
	})
}
