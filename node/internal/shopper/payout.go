package shopper

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

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

// maxDonationBPS caps the donation a release may carry (§2.3): 1% of the payout.
const maxDonationBPS = 100

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

// donation returns the donation address and bps (capped at 1%) of the list the order was placed under (§2.3):
// the list of the request's operator, and only while that operator's row for this order is effective (delegated
// and not revoked). A list of any other operator is never used.
func (e *Engine) donation(o *Order) (btcAddr, evmAddr string, bps int64) {
	me := e.Keys.NostrPubHex()
	host := ""
	if o.Shop != nil {
		host = o.Shop.Host
	}
	rows := trust.Find(e.Trust.Effective(e.Coordinators, e.Network),
		trust.Match{Shopper: me, Escrow: o.Request.Escrow, Region: o.Request.ShopRegion, Payment: o.Request.Payment, Host: host}, o.Request.Operator)
	i := slices.IndexFunc(rows, func(r trust.Row) bool { return r.Operator == o.Request.Operator })
	if i < 0 {
		return "", "", 0
	}
	ev := e.Trust.Get(trust.Key{Kind: trust.KindList, PubKey: rows[i].Operator, D: e.Network})
	if ev == nil || ev.ID != rows[i].ListID {
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
	return d.BTCAddress, d.EVMAddress, min(d.BPS, maxDonationBPS)
}

// onRelease records the user's partially signed release; the pending action countersigns and broadcasts it.
func (e *Engine) onRelease(ctx context.Context, msg *messenger.Message) {
	var r proto.Release
	if err := msg.Decode(&r); err != nil {
		return
	}
	_, err := e.update(msg.OrderID, func(o *Order) error {
		if o.User != msg.From {
			return errSkip
		}
		if !o.funded() {
			e.log.Warn("release for an order without open escrow", "order", o.ID, "state", o.State)
			return errSkip
		}
		if cur := o.Pending[ActRelease]; cur != nil && cur.Tx != "" {
			return errSkip // a release is already on its way to the chain
		}
		o.addPending(ActRelease, &Action{Event: msg.Inner})
		o.note("release received")
		return nil
	})
	if err == nil {
		e.kick(ctx, msg.OrderID)
	}
}

// payout is a transaction ready to be sent: a finalized BTC transaction, or an EVM call.
type payout struct {
	tx   *wire.MsgTx
	to   common.Address
	data []byte
}

func (e *Engine) prepareRelease(ctx context.Context, o *Order, a *Action) (*payout, error) {
	var r proto.Release
	if err := json.Unmarshal([]byte(a.Event.Content), &r); err != nil {
		return nil, contract.Definite(err)
	}
	switch o.Quote.Asset {
	case proto.AssetBTC:
		if r.PSBT == "" {
			return nil, contract.Mismatchf("release without psbt")
		}
		esc, err := contract.BTCEscrow(&o.Request, o.Quote)
		if err != nil {
			return nil, contract.Definite(err)
		}
		p, err := btc.DecodePSBT(r.PSBT)
		if err != nil {
			return nil, contract.Definite(err)
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
			return nil, contract.Definite(err)
		}
		return e.signBTC(o, esc, p)
	case proto.AssetUSDC:
		if r.SafeTx == nil {
			return nil, contract.Mismatchf("release without safe_tx")
		}
		d, err := e.Deployments()
		if err != nil {
			return nil, err
		}
		want := map[common.Address]*big.Int{common.HexToAddress(o.Quote.ShopperEVMAddress): o.lock()}
		return e.prepareSafe(ctx, o, d, *r.SafeTx, r.Signature, want, common.HexToAddress(o.Request.UserEVMAddress))
	}
	return nil, contract.Mismatchf("unknown asset %q", o.Quote.Asset)
}

// signBTC adds our signature to a checked 2-of-3 PSBT and finalizes it.
func (e *Engine) signBTC(o *Order, esc btc.Escrow, p *psbt.Packet) (*payout, error) {
	key, err := e.Keys.OrderKey(o.ID)
	if err != nil {
		return nil, contract.Definite(err)
	}
	if err := btc.Sign(p, key); err != nil {
		return nil, contract.Definite(err)
	}
	tx, err := esc.Finalize(p, btc.PathMultisig)
	if err != nil {
		return nil, contract.Definite(err)
	}
	return &payout{tx: tx}, nil
}

// prepareSafe checks a SafeTx signed by the counterparty and signs it; we execute it (and pay the gas).
func (e *Engine) prepareSafe(ctx context.Context, o *Order, d *evm.Deployments, t evm.SafeTx, sigHex string, want map[common.Address]*big.Int, signer common.Address) (*payout, error) {
	sig, err := contract.ParseSig(sigHex)
	if err != nil {
		return nil, contract.Definite(err)
	}
	safe := common.HexToAddress(o.Safe)
	nonce, err := e.EVM.SafeNonce(ctx, safe)
	if err != nil {
		return nil, err
	}
	if err := contract.CheckSafePayout(t, d, d.ChainID, safe, nonce, want, sig, signer); err != nil {
		return nil, contract.Definite(err)
	}
	mine, err := evm.Sign(t.Hash(d.ChainID, safe), e.Keys.EVM)
	if err != nil {
		return nil, contract.Definite(err)
	}
	data, err := evm.ExecTransactionCall(t, map[common.Address][]byte{signer: sig, e.Keys.EVMAddress(): mine})
	if err != nil {
		return nil, contract.Definite(err)
	}
	return &payout{to: safe, data: data}, nil
}

// offerRefund sends a cooperative refund (order.refund) signed by us after a failed purchase.
func (e *Engine) offerRefund(ctx context.Context, o *Order) error {
	if !o.funded() {
		return contract.Mismatchf("the escrow is no longer open")
	}
	body := proto.Release{Asset: o.Quote.Asset}
	switch o.Quote.Asset {
	case proto.AssetBTC:
		esc, err := contract.BTCEscrow(&o.Request, o.Quote)
		if err != nil {
			return contract.Definite(err)
		}
		p, err := esc.NewSpend(*o.Outpoint, []btc.Output{{Address: o.Request.UserBTCAddress, Amount: o.Outpoint.Amount - o.reserve()}}, btc.PathMultisig)
		if err != nil {
			return contract.Definite(err)
		}
		key, err := e.Keys.OrderKey(o.ID)
		if err != nil {
			return contract.Definite(err)
		}
		if err := btc.Sign(p, key); err != nil {
			return contract.Definite(err)
		}
		if body.PSBT, err = btc.EncodePSBT(p); err != nil {
			return contract.Definite(err)
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
			return contract.Definite(err)
		}
		sig, err := evm.Sign(t.Hash(d.ChainID, safe), e.Keys.EVM)
		if err != nil {
			return contract.Definite(err)
		}
		body.SafeTx, body.Signature = &t, "0x"+hex.EncodeToString(sig)
	}
	_, err := e.send(ctx, o, o.User, proto.TypeOrderRefund, body)
	return err
}

// onRuling records a ruling of the order's escrow. We countersign it automatically only for an open dispute,
// and only if our policy agrees; the checks of the payout itself run when the pending action is carried out.
func (e *Engine) onRuling(ctx context.Context, msg *messenger.Message) {
	var r proto.Ruling
	if err := msg.Decode(&r); err != nil {
		return
	}
	_, err := e.update(msg.OrderID, func(o *Order) error {
		if msg.From != o.Request.Escrow {
			return errSkip
		}
		if prev := o.Events["ruling"]; prev != nil {
			if prev.ID != msg.Inner.ID {
				o.note("a second ruling of the escrow was ignored")
				return nil
			}
			return errSkip
		}
		o.Ruling = &r
		o.Events["ruling"] = msg.Inner
		o.note(fmt.Sprintf("ruling user=%s shopper=%s fee=%s: %s", r.Split.User, r.Split.Shopper, r.Split.EscrowFee, r.Reason))
		if why := e.declineRuling(o, &r); why != "" {
			o.note("ruling not countersigned: " + why)
			return nil
		}
		o.addPending(ActRuling, &Action{Event: msg.Inner})
		return nil
	})
	if err == nil {
		e.kick(ctx, msg.OrderID)
	}
}

// declineRuling tells why we do not countersign a ruling ("" = we do).
func (e *Engine) declineRuling(o *Order, r *proto.Ruling) string {
	switch {
	case !o.funded():
		return "the escrow is not open"
	case o.Dispute == nil:
		return "there is no open dispute" // §4.8: a ruling nobody asked for is not countersigned
	}
	shopperShare, err := amount.ParseInt(r.Split.Shopper)
	if err != nil {
		return "split.shopper: " + err.Error()
	}
	if shopperShare.Sign() == 0 {
		return "nothing for us; the user countersigns"
	}
	if userShare, _ := amount.ParseInt(r.Split.User); e.cfg.AcceptRulings == "favorable" && userShare != nil && shopperShare.Cmp(userShare) < 0 {
		return "policy favorable"
	}
	return ""
}

func (e *Engine) prepareRuling(ctx context.Context, o *Order, a *Action) (*payout, error) {
	var r proto.Ruling
	if err := json.Unmarshal([]byte(a.Event.Content), &r); err != nil {
		return nil, contract.Definite(err)
	}
	parts := map[string]*big.Int{}
	for name, s := range map[string]string{"user": r.Split.User, "shopper": r.Split.Shopper, "escrow_fee": r.Split.EscrowFee} {
		v, err := amount.ParseInt(s)
		if err != nil {
			return nil, contract.Mismatchf("split.%s: %v", name, err)
		}
		parts[name] = v
	}
	sum := new(big.Int).Add(parts["user"], parts["shopper"])
	sum.Add(sum, parts["escrow_fee"])
	escrow, _ := e.Trust.EscrowProfile(o.Request.Escrow, e.Network)
	if escrow == nil {
		return nil, contract.Mismatchf("no profile of escrow %s to check its dispute fee", short(o.Request.Escrow))
	}
	// escrow_fee ≤ dispute_fee_bps of what is divided (§4.8)
	if max := amount.BPS(sum, escrow.DisputeFeeBPS); parts["escrow_fee"].Cmp(max) > 0 {
		return nil, contract.Mismatchf("escrow fee %s exceeds %d bps of %s", parts["escrow_fee"], escrow.DisputeFeeBPS, sum)
	}
	switch o.Quote.Asset {
	case proto.AssetBTC:
		if r.PSBT == "" {
			return nil, contract.Mismatchf("ruling without psbt")
		}
		if want := o.Outpoint.Amount - o.reserve(); sum.Int64() != want {
			return nil, contract.Mismatchf("split sums to %s, escrow holds %d after the reserve", sum, want)
		}
		esc, err := contract.BTCEscrow(&o.Request, o.Quote)
		if err != nil {
			return nil, contract.Definite(err)
		}
		p, err := btc.DecodePSBT(r.PSBT)
		if err != nil {
			return nil, contract.Definite(err)
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
			return nil, contract.Definite(err)
		}
		return e.signBTC(o, esc, p)
	case proto.AssetUSDC:
		if r.SafeTx == nil {
			return nil, contract.Mismatchf("ruling without safe_tx")
		}
		d, err := e.Deployments()
		if err != nil {
			return nil, err
		}
		bal, err := e.EVM.BalanceOf(ctx, d.USDC, common.HexToAddress(o.Safe))
		if err != nil {
			return nil, err
		}
		if sum.Cmp(bal) != 0 {
			return nil, contract.Mismatchf("split sums to %s, safe holds %s", sum, bal)
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
		return e.prepareSafe(ctx, o, d, *r.SafeTx, r.Signature, want, common.HexToAddress(o.Quote.EscrowEVMAddress))
	}
	return nil, contract.Mismatchf("unknown asset %q", o.Quote.Asset)
}

// onCountersigned notes the payout the user says it countersigned. Nothing changes until the chain shows the
// escrow spent (watchEscrows); a message alone never ends an order (§4.8, §4.10).
func (e *Engine) onCountersigned(ctx context.Context, msg *messenger.Message) {
	var ref proto.TxRef
	if msg.Decode(&ref) != nil || ref.TxID == "" || len(ref.TxID) > 66 {
		return
	}
	_, _ = e.update(msg.OrderID, func(o *Order) error {
		if msg.From != o.User || o.PayoutTx != "" {
			return errSkip
		}
		if o.Ruling == nil {
			o.note("dispute.countersigned without a ruling, ignored")
			return nil
		}
		o.ClaimedPayout = ref.TxID
		o.note("the user countersigned the ruling (" + ref.TxID + "); waiting for the chain")
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

// claimable: a delivered order whose user neither released nor had a ruling carried out by T1. An order the
// escrow ruled is not claimed: the ruling decides it.
func (o *Order) claimable() bool {
	return o.ShipStatus == "delivered" && o.PayoutTx == "" && o.Ruling == nil
}

func (e *Engine) watchBTC(ctx context.Context, o *Order) {
	spent, err := e.BTC.Outspend(ctx, o.Outpoint.TxID, o.Outpoint.Vout)
	if err != nil {
		return
	}
	if spent.Spent {
		e.spent(ctx, o.ID, spent.TxID)
		return
	}
	if !o.claimable() || o.Pending[ActClaim] != nil {
		return
	}
	tip, err := e.BTC.TipHeight(ctx)
	if err != nil || tip < o.Quote.Timelock.T1 {
		return
	}
	e.scheduleClaim(ctx, o.ID)
}

func (e *Engine) watchSafe(ctx context.Context, o *Order) {
	d, err := e.Deployments()
	if err != nil {
		return
	}
	bal, err := e.EVM.BalanceOf(ctx, d.USDC, common.HexToAddress(o.Safe))
	if err != nil {
		return
	}
	if bal.Sign() == 0 {
		e.spent(ctx, o.ID, "")
		return
	}
	if !o.claimable() || o.Pending[ActClaim] != nil {
		return
	}
	now, err := e.EVM.LatestTime(ctx)
	if err != nil || int64(now) < o.Quote.Timelock.T1 {
		return
	}
	e.scheduleClaim(ctx, o.ID)
}

// scheduleClaim adds the T1 claim as a pending action, so that a claim already sent is waited for instead of
// being sent again on the next tick.
func (e *Engine) scheduleClaim(ctx context.Context, id string) {
	_, err := e.update(id, func(o *Order) error {
		if !o.funded() || !o.claimable() || o.Pending[ActClaim] != nil {
			return errSkip
		}
		o.addPending(ActClaim, &Action{})
		return nil
	})
	if err == nil {
		e.kick(ctx, id)
	}
}

func (e *Engine) prepareClaim(ctx context.Context, o *Order) (*payout, error) {
	switch o.Quote.Asset {
	case proto.AssetBTC:
		esc, err := contract.BTCEscrow(&o.Request, o.Quote)
		if err != nil {
			return nil, contract.Definite(err)
		}
		p, err := esc.NewSpend(*o.Outpoint, []btc.Output{{Address: o.Quote.ShopperBTCAddress, Amount: o.Outpoint.Amount - o.reserve()}}, btc.PathShopperT1)
		if err != nil {
			return nil, contract.Definite(err)
		}
		key, err := e.Keys.OrderKey(o.ID)
		if err != nil {
			return nil, contract.Definite(err)
		}
		if err := btc.Sign(p, key); err != nil {
			return nil, contract.Definite(err)
		}
		tx, err := esc.Finalize(p, btc.PathShopperT1)
		if err != nil {
			return nil, contract.Definite(err)
		}
		return &payout{tx: tx}, nil
	case proto.AssetUSDC:
		d, err := e.Deployments()
		if err != nil {
			return nil, err
		}
		data, err := evm.ClaimByShopperCall(common.HexToAddress(o.Safe))
		if err != nil {
			return nil, contract.Definite(err)
		}
		return &payout{to: d.Module, data: data}, nil
	}
	return nil, contract.Mismatchf("unknown asset %q", o.Quote.Asset)
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

// spent handles an escrow the chain shows spent (txid: the spending BTC transaction; "" for an emptied Safe).
// A payout of our own that is still pending is finished by its action; a ruling the user said it countersigned
// settles the order once the chain confirms it; anything else closes the order.
func (e *Engine) spent(ctx context.Context, id, txid string) {
	defer e.lock(id)()
	o, ok, err := e.Order(id)
	if err != nil || !ok || o.PayoutTx != "" || terminal(o.State) {
		return
	}
	for _, a := range o.Pending {
		if a.Tx != "" && (txid == "" || a.Tx == txid) {
			e.kick(ctx, id)
			return
		}
	}
	settledBy := ""
	if o.Ruling != nil && o.ClaimedPayout != "" {
		if txid != "" && strings.EqualFold(txid, o.ClaimedPayout) {
			settledBy = txid
		} else if txid == "" && e.emptiedSafe(ctx, o, o.ClaimedPayout) {
			settledBy = o.ClaimedPayout
		}
	}
	_, _ = e.update(id, func(o *Order) error {
		if o.PayoutTx != "" || terminal(o.State) {
			return errSkip
		}
		if settledBy != "" {
			o.PayoutTx, o.PayoutBy = settledBy, "ruling"
			o.set(StateSettled, "ruling countersigned by the user: "+settledBy)
			return nil
		}
		o.PayoutTx, o.PayoutBy = txid, "other"
		if txid == "" {
			o.PayoutTx = "(safe emptied)"
		}
		o.set(StateClosed, strings.TrimSpace("escrow spent "+txid))
		return nil
	})
}

// emptiedSafe tells whether hash is a successful transaction moving USDC out of the order's Safe.
func (e *Engine) emptiedSafe(ctx context.Context, o *Order, hash string) bool {
	d, err := e.Deployments()
	if err != nil {
		return false
	}
	r, err := e.EVM.Receipt(ctx, common.HexToHash(hash))
	if err != nil || r.Status != types.ReceiptStatusSuccessful {
		return false
	}
	for _, t := range evm.TransfersOf(r, d.USDC) {
		if t.From == common.HexToAddress(o.Safe) {
			return true
		}
	}
	return false
}

// errWaiting means a sent transaction is not mined yet.
var errWaiting = errors.New("transaction sent, waiting for it to be mined")

// txPatience is how long a sent EVM transaction may stay unmined before it is sent again.
const txPatience = 10 * time.Minute

// submit sends a prepared payout. The transaction id is stored on the action before it is sent, so that after
// a crash or a lost answer the next attempt looks for it instead of sending another one.
func (e *Engine) submit(ctx context.Context, o *Order, kind string, p *payout) (string, error) {
	if p.tx != nil {
		txid := p.tx.TxHash().String()
		if err := e.setTx(o.ID, kind, txid); err != nil {
			return "", err
		}
		if _, err := e.BTC.Broadcast(ctx, btc.TxHex(p.tx)); err != nil {
			// already broadcast (by an earlier attempt) counts as sent
			if _, terr := e.BTC.Tx(ctx, txid); terr == nil {
				return txid, nil
			}
			return "", fmt.Errorf("broadcast: %w", err)
		}
		return txid, nil
	}
	hash, err := e.EVM.Send(ctx, e.Keys.EVM, p.to, p.data, nil)
	if err != nil {
		return "", err
	}
	if err := e.setTx(o.ID, kind, hash.Hex()); err != nil {
		return "", err
	}
	wctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if _, err := e.EVM.Wait(wctx, hash); err != nil {
		return "", err
	}
	return hash.Hex(), nil
}

func (e *Engine) setTx(id, kind, tx string) error {
	_, err := e.update(id, func(o *Order) error {
		a := o.Pending[kind]
		if a == nil {
			return errSkip
		}
		a.Tx, a.Sent = tx, time.Now().Unix()
		return nil
	})
	if errors.Is(err, errSkip) {
		return nil
	}
	return err
}

// sentTx checks the transaction an earlier attempt sent. done: it is on the chain (BTC: known to Esplora; EVM:
// mined and successful). Otherwise the action either waits (errWaiting) or sends anew (Tx cleared).
func (e *Engine) sentTx(ctx context.Context, o *Order, kind string, a *Action) (done bool, err error) {
	if o.Quote.Asset == proto.AssetBTC {
		if _, err := e.BTC.Tx(ctx, a.Tx); err == nil {
			return true, nil
		} else if !btc.IsNotFound(err) {
			return false, err
		}
		return false, nil // rebuilt and broadcast again; the same inputs give the same transaction
	}
	r, err := e.EVM.Receipt(ctx, common.HexToHash(a.Tx))
	switch {
	case errors.Is(err, ethereum.NotFound):
		if time.Since(time.Unix(a.Sent, 0)) < txPatience {
			return false, errWaiting
		}
	case err != nil:
		return false, err
	case r.Status == types.ReceiptStatusSuccessful:
		return true, nil
	}
	// reverted, or dropped for too long: start over (the checks see a moved nonce or an empty escrow)
	a.Tx = ""
	return false, e.setTx(o.ID, kind, "")
}
