// Package contract ties an order's messages to its on-chain escrow: it rebuilds the P2WSH script or the Safe from
// order.request + order.quote and checks the funding of §4.6.
package contract

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/amount"
	"github.com/pad01g/proxy-shopping-go/node/internal/btc"
	"github.com/pad01g/proxy-shopping-go/node/internal/evm"
	"github.com/pad01g/proxy-shopping-go/node/internal/giftwrap"
	"github.com/pad01g/proxy-shopping-go/node/internal/keys"
	"github.com/pad01g/proxy-shopping-go/node/internal/proto"
)

// Order is the signed agreement of an order: request and accept by the user, quote by the shopper, and
// (once paid) funded by the user.
type Order struct {
	ID      string
	User    string
	Shopper string

	Request *proto.OrderRequest
	Quote   *proto.OrderQuote
	Funded  *proto.OrderFunded

	RequestEvent, QuoteEvent, AcceptEvent, FundedEvent *nostr.Event
}

// FromEvents assembles an order from signed inner events (e.g. an escrow.notice or dispute evidence) and checks
// that they belong together: same order id, request/accept/funded by the user, quote by the shopper, the quote
// answers the request and the accept names the quote.
func FromEvents(evs []*nostr.Event) (*Order, error) {
	o := &Order{}
	for _, ev := range evs {
		if ev == nil {
			continue
		}
		if err := giftwrap.VerifyInner(ev); err != nil {
			return nil, err
		}
		switch giftwrap.Type(ev) {
		case proto.TypeOrderRequest:
			o.RequestEvent = ev
		case proto.TypeOrderQuote:
			o.QuoteEvent = ev
		case proto.TypeOrderAccept:
			o.AcceptEvent = ev
		case proto.TypeOrderFunded:
			o.FundedEvent = ev
		}
	}
	if o.RequestEvent == nil || o.QuoteEvent == nil {
		return nil, errors.New("request and quote are needed")
	}
	o.ID = giftwrap.OrderID(o.RequestEvent)
	o.User = o.RequestEvent.PubKey
	o.Shopper = giftwrap.Recipient(o.RequestEvent)
	if _, err := keys.OrderIDBytes(o.ID); err != nil {
		return nil, err
	}
	if o.QuoteEvent.PubKey != o.Shopper || giftwrap.Recipient(o.QuoteEvent) != o.User || giftwrap.OrderID(o.QuoteEvent) != o.ID {
		return nil, errors.New("quote does not answer the request")
	}
	o.Request, o.Quote = new(proto.OrderRequest), new(proto.OrderQuote)
	if err := json.Unmarshal([]byte(o.RequestEvent.Content), o.Request); err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	if err := json.Unmarshal([]byte(o.QuoteEvent.Content), o.Quote); err != nil {
		return nil, fmt.Errorf("quote: %w", err)
	}
	if a := o.AcceptEvent; a != nil {
		var acc proto.OrderAccept
		if a.PubKey != o.User || giftwrap.OrderID(a) != o.ID || json.Unmarshal([]byte(a.Content), &acc) != nil || acc.QuoteID != o.QuoteEvent.ID {
			return nil, errors.New("accept does not name the quote")
		}
	}
	if f := o.FundedEvent; f != nil {
		if f.PubKey != o.User || giftwrap.OrderID(f) != o.ID {
			return nil, errors.New("funded is not the user's")
		}
		o.Funded = new(proto.OrderFunded)
		if err := json.Unmarshal([]byte(f.Content), o.Funded); err != nil {
			return nil, fmt.Errorf("funded: %w", err)
		}
	}
	return o, nil
}

// Events lists the signed events of the order.
func (o *Order) Events() []*nostr.Event {
	var out []*nostr.Event
	for _, ev := range []*nostr.Event{o.RequestEvent, o.QuoteEvent, o.AcceptEvent, o.FundedEvent} {
		if ev != nil {
			out = append(out, ev)
		}
	}
	return out
}

// BTCEscrow rebuilds the P2WSH contract of a BTC order.
func BTCEscrow(req *proto.OrderRequest, q *proto.OrderQuote) (btc.Escrow, error) {
	var e btc.Escrow
	if q.Timelock == nil {
		return e, errors.New("quote without timelock")
	}
	var err error
	if e.User, err = keys.ParsePubKeyHex(req.UserBTCPubkey); err != nil {
		return e, fmt.Errorf("user_btc_pubkey: %w", err)
	}
	if e.Shopper, err = keys.ParsePubKeyHex(q.ShopperBTCPubkey); err != nil {
		return e, fmt.Errorf("shopper_btc_pubkey: %w", err)
	}
	if e.Escrow, err = keys.ParsePubKeyHex(q.EscrowBTCPubkey); err != nil {
		return e, fmt.Errorf("escrow_btc_pubkey: %w", err)
	}
	if q.Timelock.T1 <= 0 || q.Timelock.T2 <= q.Timelock.T1 || q.Timelock.T2 >= 500000000 {
		return e, fmt.Errorf("bad timelock %+v", *q.Timelock)
	}
	e.T1, e.T2 = uint32(q.Timelock.T1), uint32(q.Timelock.T2)
	return e, nil
}

// SafeOf rebuilds the Safe of a USDC order.
func SafeOf(d *evm.Deployments, req *proto.OrderRequest, q *proto.OrderQuote) (evm.OrderSafe, error) {
	if q.Timelock == nil {
		return evm.OrderSafe{}, errors.New("quote without timelock")
	}
	for name, a := range map[string]string{"user_evm_address": req.UserEVMAddress, "shopper_evm_address": q.ShopperEVMAddress, "escrow_evm_address": q.EscrowEVMAddress} {
		if !common.IsHexAddress(a) {
			return evm.OrderSafe{}, fmt.Errorf("%s %q is not an address", name, a)
		}
	}
	if q.Timelock.T1 <= 0 || q.Timelock.T2 <= q.Timelock.T1 {
		return evm.OrderSafe{}, fmt.Errorf("bad timelock %+v", *q.Timelock)
	}
	return evm.NewOrderSafe(d, common.HexToAddress(req.UserEVMAddress), common.HexToAddress(q.ShopperEVMAddress),
		common.HexToAddress(q.EscrowEVMAddress), uint64(q.Timelock.T1), uint64(q.Timelock.T2)), nil
}

// BTCChain is what funding checks need from Esplora.
type BTCChain interface {
	Tx(ctx context.Context, txid string) (*btc.Tx, error)
	Confirmations(ctx context.Context, txid string) (int64, error)
}

// ErrNotYet means the funding is not (yet) visible or confirmed; retry later.
var ErrNotYet = errors.New("funding not confirmed yet")

// VerifyBTCFunding checks §4.6 for BTC: the output pays the escrow address the lock amount and the escrow fee
// address got its upfront fee, with enough confirmations. It returns the funded outpoint.
func VerifyBTCFunding(ctx context.Context, chain BTCChain, esc btc.Escrow, q *proto.OrderQuote, f *proto.OrderFunded, minConf int64) (btc.Outpoint, error) {
	var out btc.Outpoint
	if f == nil || f.Asset != proto.AssetBTC || f.TxID == "" || f.Vout == nil {
		return out, errors.New("funded needs txid and vout")
	}
	addr, err := esc.Address()
	if err != nil {
		return out, err
	}
	if q.EscrowAddress != "" && q.EscrowAddress != addr {
		return out, fmt.Errorf("quoted escrow address %s differs from the script address %s", q.EscrowAddress, addr)
	}
	lock, err := amount.ParseInt(q.LockAmount)
	if err != nil {
		return out, err
	}
	tx, err := chain.Tx(ctx, f.TxID)
	if btc.IsNotFound(err) {
		return out, ErrNotYet
	}
	if err != nil {
		return out, err
	}
	if int(*f.Vout) >= len(tx.Vout) {
		return out, fmt.Errorf("funding tx has no output %d", *f.Vout)
	}
	o := tx.Vout[*f.Vout]
	if o.ScriptPubKeyAddress != addr {
		return out, fmt.Errorf("output %d pays %s, not the escrow %s", *f.Vout, o.ScriptPubKeyAddress, addr)
	}
	if big.NewInt(o.Value).Cmp(lock) < 0 {
		return out, fmt.Errorf("output %d locks %d sats, quote wants %s", *f.Vout, o.Value, q.LockAmount)
	}
	if err := verifyBTCFee(ctx, chain, tx, q, f); err != nil {
		return out, err
	}
	conf, err := chain.Confirmations(ctx, f.TxID)
	if err != nil {
		return out, err
	}
	if conf < minConf {
		return out, ErrNotYet
	}
	return btc.Outpoint{TxID: f.TxID, Vout: *f.Vout, Amount: o.Value}, nil
}

func verifyBTCFee(ctx context.Context, chain BTCChain, fundingTx *btc.Tx, q *proto.OrderQuote, f *proto.OrderFunded) error {
	fee, err := amount.ParseInt(q.EscrowUpfrontFee)
	if err != nil {
		return fmt.Errorf("escrow_upfront_fee: %w", err)
	}
	if fee.Sign() == 0 {
		return nil
	}
	tx := fundingTx
	if f.FeeTxID != "" && f.FeeTxID != fundingTx.TxID {
		if tx, err = chain.Tx(ctx, f.FeeTxID); err != nil {
			return fmt.Errorf("fee tx: %w", err)
		}
	}
	var paid int64
	for _, o := range tx.Vout {
		if o.ScriptPubKeyAddress == q.EscrowBTCFeeAddress {
			paid += o.Value
		}
	}
	if big.NewInt(paid).Cmp(fee) < 0 {
		return fmt.Errorf("escrow upfront fee: %d sats paid to %s, %s due", paid, q.EscrowBTCFeeAddress, q.EscrowUpfrontFee)
	}
	return nil
}

// VerifyBTCFee checks only the upfront fee of a BTC order (the escrow's check before it takes a case).
func VerifyBTCFee(ctx context.Context, chain BTCChain, q *proto.OrderQuote, f *proto.OrderFunded) error {
	if f == nil || f.TxID == "" {
		return errors.New("no funding transaction")
	}
	id := f.FeeTxID
	if id == "" {
		id = f.TxID
	}
	tx, err := chain.Tx(ctx, id)
	if err != nil {
		return fmt.Errorf("fee tx: %w", err)
	}
	return verifyBTCFee(ctx, chain, tx, q, &proto.OrderFunded{TxID: id})
}

// VerifySafeFunding checks §4.6 for USDC: the Safe at the predicted address has the owners, threshold and
// module registration of the quote, holds the lock amount, and the fee transaction paid the escrow.
func VerifySafeFunding(ctx context.Context, c *evm.Client, d *evm.Deployments, os evm.OrderSafe, orderID string, q *proto.OrderQuote, f *proto.OrderFunded, minConf int64) (common.Address, error) {
	var zero common.Address
	if f == nil || f.Asset != proto.AssetUSDC || !common.IsHexAddress(f.Safe) {
		return zero, errors.New("funded needs the safe address")
	}
	want, err := c.PredictSafe(ctx, d, os, orderID)
	if err != nil {
		return zero, err
	}
	safe := common.HexToAddress(f.Safe)
	if safe != want || (q.EscrowAddress != "" && !strings.EqualFold(q.EscrowAddress, want.Hex())) {
		return zero, fmt.Errorf("safe %s is not the predicted %s", safe.Hex(), want.Hex())
	}
	st, err := c.InspectSafe(ctx, safe, d.Module)
	if err != nil {
		return zero, ErrNotYet
	}
	if st.Threshold.Int64() != 2 || !sameOwners(st.Owners, os.Owners()) || !st.ModuleEnabled {
		return zero, fmt.Errorf("safe %s is not the 2-of-3 of the order with the module", safe.Hex())
	}
	cfg := st.Config
	if cfg.Token != d.USDC || cfg.User != os.User || cfg.Shopper != os.Shopper || cfg.T1 != os.T1 || cfg.T2 != os.T2 {
		return zero, fmt.Errorf("module config of %s does not match the quote", safe.Hex())
	}
	lock, err := amount.ParseInt(q.LockAmount)
	if err != nil {
		return zero, err
	}
	bal, err := c.BalanceOf(ctx, d.USDC, safe)
	if err != nil {
		return zero, err
	}
	if bal.Cmp(lock) < 0 {
		return zero, ErrNotYet
	}
	if err := VerifyUSDCFee(ctx, c, d, q, f); err != nil {
		return zero, err
	}
	if minConf > 1 && f.FundTx != "" {
		r, err := c.Eth.TransactionReceipt(ctx, common.HexToHash(f.FundTx))
		if err != nil {
			return zero, ErrNotYet
		}
		head, err := c.Eth.BlockNumber(ctx)
		if err != nil {
			return zero, err
		}
		if int64(head)-r.BlockNumber.Int64()+1 < minConf {
			return zero, ErrNotYet
		}
	}
	return safe, nil
}

// VerifyUSDCFee checks that the fee transaction paid the escrow its upfront fee.
func VerifyUSDCFee(ctx context.Context, c *evm.Client, d *evm.Deployments, q *proto.OrderQuote, f *proto.OrderFunded) error {
	fee, err := amount.ParseInt(q.EscrowUpfrontFee)
	if err != nil {
		return fmt.Errorf("escrow_upfront_fee: %w", err)
	}
	if fee.Sign() == 0 {
		return nil
	}
	if f.FeeTx == "" {
		return errors.New("funded without fee_tx")
	}
	transfers, err := c.TokenTransfers(ctx, common.HexToHash(f.FeeTx), d.USDC)
	if err != nil {
		return fmt.Errorf("fee tx: %w", err)
	}
	paid := new(big.Int)
	for _, t := range transfers {
		if strings.EqualFold(t.To.Hex(), q.EscrowEVMAddress) {
			paid.Add(paid, t.Amount)
		}
	}
	if paid.Cmp(fee) < 0 {
		return fmt.Errorf("escrow upfront fee: %s paid, %s due", paid, fee)
	}
	return nil
}

func sameOwners(a, b []common.Address) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[common.Address]bool{}
	for _, x := range a {
		seen[x] = true
	}
	for _, x := range b {
		if !seen[x] {
			return false
		}
	}
	return true
}
