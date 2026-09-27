// Package contract ties an order's messages to its on-chain escrow: it rebuilds the P2WSH script or the Safe from
// order.request + order.quote and checks the funding of §4.6.
package contract

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
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
	if o.Quote.Accept && o.Quote.Asset != o.Request.Payment {
		return nil, errors.New("quote is for another payment than the request")
	}
	if err := VerifyKeyProof(o.ID, o.User, o.Request); err != nil {
		return nil, err
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
		if err := CheckFundedIDs(o.Funded); err != nil {
			return nil, fmt.Errorf("funded: %w", err)
		}
	}
	return o, nil
}

// CheckFundedIDs checks the transaction ids of an order.funded: BTC txids are 64 lowercase hex, EVM hashes 0x
// and 64 lowercase hex. One chain object has one spelling, so the one-order-per-funding records (keyed by
// these ids) cannot be dodged by writing an id in capitals.
func CheckFundedIDs(f *proto.OrderFunded) error {
	if f == nil {
		return nil
	}
	switch f.Asset {
	case proto.AssetBTC:
		for name, id := range map[string]string{"txid": f.TxID, "fee_txid": f.FeeTxID} {
			if id != "" && !IsTxID(id) {
				return Mismatchf("%s %.80q is not 64 lowercase hex", name, id)
			}
		}
	case proto.AssetUSDC:
		for name, id := range map[string]string{"deploy_tx": f.DeployTx, "fund_tx": f.FundTx, "fee_tx": f.FeeTx} {
			if id != "" && !IsTxHash(id) {
				return Mismatchf("%s %.80q is not 0x and 64 lowercase hex", name, id)
			}
		}
		if f.Safe != "" && !common.IsHexAddress(f.Safe) {
			return Mismatchf("safe %.80q is not an address", f.Safe)
		}
	default:
		return Mismatchf("unknown asset %.40q", f.Asset)
	}
	return nil
}

// IsTxID tells whether s is a BTC txid: 64 lowercase hex.
func IsTxID(s string) bool { return len(s) == 64 && isLowerHex(s) }

// IsTxHash tells whether s is an EVM transaction hash: 0x and 64 lowercase hex.
func IsTxHash(s string) bool { return len(s) == 66 && strings.HasPrefix(s, "0x") && isLowerHex(s[2:]) }

func isLowerHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ReserveCap is the largest payout_fee_reserve a user client accepts for a BTC lock (§4.5):
// min(20000 sats, max(2000 sats, 5% of the lock)).
func ReserveCap(lock int64) int64 {
	return min(20000, max(2000, lock/20))
}

// DustLimit is the smallest BTC output this implementation creates or accepts (sats).
const DustLimit = btc.DustLimit

// VerifyKeyProof checks the key_proof of a request (§4.4.1): the signer of the request holds the chain key it
// puts into the multisig.
func VerifyKeyProof(orderID, user string, req *proto.OrderRequest) error {
	if req.KeyProof == "" {
		return errors.New("request without key_proof")
	}
	var err error
	switch req.Payment {
	case proto.AssetBTC:
		err = keys.VerifyKeyProofBTC(req.UserBTCPubkey, orderID, user, req.KeyProof)
	case proto.AssetUSDC:
		err = keys.VerifyKeyProofEVM(req.UserEVMAddress, orderID, user, req.KeyProof)
	default:
		err = fmt.Errorf("unknown payment %q", req.Payment)
	}
	if err != nil {
		return fmt.Errorf("key_proof: %w", err)
	}
	return nil
}

// Select picks the signed agreement of an order for the escrow (§4.7). The messages of the escrow.notice win;
// evidence messages of the same type with another id are ignored (and listed in ignored). Without a notice
// the agreement comes from the evidence, which must not contain two different requests; of several quotes
// the one named by an accept is used, and of several fundings the latest.
func Select(orderID string, notice, evidence []*nostr.Event) (evs []*nostr.Event, ignored []string, err error) {
	byType := map[string][]*nostr.Event{}
	add := func(ev *nostr.Event) {
		if ev == nil || giftwrap.OrderID(ev) != orderID || giftwrap.VerifyInner(ev) != nil {
			return
		}
		t := giftwrap.Type(ev)
		for _, have := range byType[t] {
			if have.ID == ev.ID {
				return
			}
		}
		byType[t] = append(byType[t], ev)
	}
	if len(notice) > 0 {
		have := map[string]string{}
		for _, ev := range notice {
			if ev != nil {
				have[giftwrap.Type(ev)] = ev.ID
			}
		}
		for _, ev := range evidence {
			if ev == nil || giftwrap.OrderID(ev) != orderID {
				continue
			}
			if id, ok := have[giftwrap.Type(ev)]; ok && id != ev.ID {
				ignored = append(ignored, giftwrap.Type(ev)+" "+ev.ID)
			}
		}
		return notice, ignored, nil
	}
	for _, ev := range evidence {
		switch giftwrap.Type(ev) {
		case proto.TypeOrderRequest, proto.TypeOrderQuote, proto.TypeOrderAccept, proto.TypeOrderFunded:
			add(ev)
		}
	}
	if n := len(byType[proto.TypeOrderRequest]); n != 1 {
		return nil, nil, fmt.Errorf("%d different requests for order %s", n, orderID)
	}
	quote := only(byType[proto.TypeOrderQuote])
	var accept *nostr.Event
	for _, a := range byType[proto.TypeOrderAccept] {
		var acc proto.OrderAccept
		if json.Unmarshal([]byte(a.Content), &acc) != nil {
			continue
		}
		for _, q := range byType[proto.TypeOrderQuote] {
			if q.ID == acc.QuoteID && (quote == nil || quote.ID == q.ID) {
				quote, accept = q, a
			}
		}
	}
	if quote == nil {
		return nil, nil, errors.New("no quote that the user accepted")
	}
	var funded *nostr.Event
	for _, f := range byType[proto.TypeOrderFunded] {
		if funded == nil || f.CreatedAt >= funded.CreatedAt {
			funded = f
		}
	}
	return []*nostr.Event{byType[proto.TypeOrderRequest][0], quote, accept, funded}, nil, nil
}

func only(evs []*nostr.Event) *nostr.Event {
	if len(evs) == 1 {
		return evs[0]
	}
	return nil
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

// Mismatch is a definite answer: the chain shows something other than what the order needs. Every other error
// of the checks (network trouble, a 5xx, an RPC error) may go away and is worth a retry.
type Mismatch struct{ Err error }

func (m *Mismatch) Error() string { return m.Err.Error() }
func (m *Mismatch) Unwrap() error { return m.Err }

// Mismatchf returns a *Mismatch.
func Mismatchf(format string, args ...any) error { return &Mismatch{Err: fmt.Errorf(format, args...)} }

// Definite marks err as a definite mismatch (nil stays nil).
func Definite(err error) error {
	if err == nil || IsDefinite(err) {
		return err
	}
	return &Mismatch{Err: err}
}

// IsDefinite tells a definite mismatch from trouble worth a retry.
func IsDefinite(err error) bool {
	var m *Mismatch
	return errors.As(err, &m)
}

// Funding is a verified funding of §4.6.
type Funding struct {
	Outpoint    btc.Outpoint   // BTC
	Safe        common.Address // USDC
	ConfirmedAt int64          // block time of the funding (UNIX seconds)
	// Uses are the keys of the chain objects the funding consumes (outpoint, Safe, fee tx); one order each.
	Uses []string
}

// VerifyBTCFunding checks §4.6 for BTC: the output pays the escrow address the lock amount and the same
// transaction pays the escrow fee address its upfront fee, with enough confirmations.
func VerifyBTCFunding(ctx context.Context, chain BTCChain, esc btc.Escrow, q *proto.OrderQuote, f *proto.OrderFunded, minConf int64) (Funding, error) {
	var out Funding
	if f != nil && f.FeeTxID != "" && f.FeeTxID != f.TxID {
		return out, Mismatchf("the upfront fee must be paid in the funding transaction (fee_txid %s, txid %s)", f.FeeTxID, f.TxID)
	}
	tx, o, err := BTCEscrowOutput(ctx, chain, esc, q, f, minConf)
	if err != nil {
		return out, err
	}
	if err := BTCFeePaid(tx, q); err != nil {
		return out, err
	}
	return Funding{
		Outpoint: btc.Outpoint{TxID: f.TxID, Vout: *f.Vout, Amount: o.Value}, ConfirmedAt: tx.Status.BlockTime,
		Uses: []string{fmt.Sprintf("btc:%s:%d", f.TxID, *f.Vout), "btc-fee:" + f.TxID},
	}, nil
}

// BTCEscrowOutput checks the escrow output an order.funded names: output vout of txid pays the P2WSH rebuilt from
// the request and quote at least the lock amount, with minConf confirmations. It returns the funding
// transaction (the upfront fee must be paid in it, §4.6) and the output.
func BTCEscrowOutput(ctx context.Context, chain BTCChain, esc btc.Escrow, q *proto.OrderQuote, f *proto.OrderFunded, minConf int64) (*btc.Tx, btc.TxOut, error) {
	if f == nil || f.Asset != proto.AssetBTC || f.TxID == "" || f.Vout == nil {
		return nil, btc.TxOut{}, Mismatchf("funded needs txid and vout")
	}
	if err := CheckFundedIDs(f); err != nil {
		return nil, btc.TxOut{}, err
	}
	addr, err := esc.Address()
	if err != nil {
		return nil, btc.TxOut{}, Definite(err)
	}
	if q.EscrowAddress != "" && q.EscrowAddress != addr {
		return nil, btc.TxOut{}, Mismatchf("quoted escrow address %s differs from the script address %s", q.EscrowAddress, addr)
	}
	lock, err := amount.ParseInt(q.LockAmount)
	if err != nil {
		return nil, btc.TxOut{}, Definite(err)
	}
	tx, err := chain.Tx(ctx, f.TxID)
	if btc.IsNotFound(err) {
		return nil, btc.TxOut{}, ErrNotYet
	}
	if err != nil {
		return nil, btc.TxOut{}, err
	}
	o, err := EscrowOutput(tx, *f.Vout, addr)
	if err != nil {
		return nil, btc.TxOut{}, err
	}
	if big.NewInt(o.Value).Cmp(lock) < 0 {
		return nil, btc.TxOut{}, Mismatchf("output %d locks %d sats, quote wants %s", *f.Vout, o.Value, q.LockAmount)
	}
	if minConf < 1 {
		minConf = 1
	}
	if !tx.Status.Confirmed {
		return nil, btc.TxOut{}, ErrNotYet
	}
	conf, err := chain.Confirmations(ctx, f.TxID)
	if err != nil {
		return nil, btc.TxOut{}, err
	}
	if conf < minConf {
		return nil, btc.TxOut{}, ErrNotYet
	}
	return tx, o, nil
}

// EscrowOutput returns output vout of tx after checking that it pays the P2WSH address of the order.
func EscrowOutput(tx *btc.Tx, vout uint32, addr string) (btc.TxOut, error) {
	if int(vout) >= len(tx.Vout) {
		return btc.TxOut{}, Mismatchf("funding tx has no output %d", vout)
	}
	o := tx.Vout[vout]
	want, err := btc.PkScript(addr)
	if err != nil {
		return o, Definite(err)
	}
	if o.ScriptPubKey != hex.EncodeToString(want) {
		return o, Mismatchf("output %d pays %s, not the escrow %s", vout, o.ScriptPubKeyAddress, addr)
	}
	return o, nil
}

// BTCFeePaid checks that tx pays escrow_btc_fee_address at least escrow_upfront_fee (§4.6). A fee of 0 passes.
func BTCFeePaid(tx *btc.Tx, q *proto.OrderQuote) error {
	fee, err := amount.ParseInt(q.EscrowUpfrontFee)
	if err != nil {
		return Mismatchf("escrow_upfront_fee: %v", err)
	}
	if fee.Sign() == 0 {
		return nil
	}
	want, err := btc.PkScript(q.EscrowBTCFeeAddress)
	if err != nil {
		return Mismatchf("escrow_btc_fee_address: %v", err)
	}
	var paid int64
	for _, o := range tx.Vout {
		if o.ScriptPubKey == hex.EncodeToString(want) {
			paid += o.Value
		}
	}
	if big.NewInt(paid).Cmp(fee) < 0 {
		return Mismatchf("escrow upfront fee: %d sats paid to %s, %s due", paid, q.EscrowBTCFeeAddress, q.EscrowUpfrontFee)
	}
	return nil
}

// CheckSafe checks that safe is the predicted Safe of the order with the owners, threshold and module
// registration of the quote.
func CheckSafe(ctx context.Context, c *evm.Client, d *evm.Deployments, os evm.OrderSafe, orderID string, q *proto.OrderQuote, safe common.Address) error {
	want, err := c.PredictSafe(ctx, d, os, orderID)
	if err != nil {
		return err
	}
	if safe != want || (q.EscrowAddress != "" && !strings.EqualFold(q.EscrowAddress, want.Hex())) {
		return Mismatchf("safe %s is not the predicted %s", safe.Hex(), want.Hex())
	}
	st, err := c.InspectSafe(ctx, safe, d.Module)
	if errors.Is(err, evm.ErrNoContract) {
		return ErrNotYet
	}
	if err != nil {
		return err
	}
	if st.Threshold.Int64() != 2 || !sameOwners(st.Owners, os.Owners()) || !st.ModuleEnabled {
		return Mismatchf("safe %s is not the 2-of-3 of the order with the module", safe.Hex())
	}
	cfg := st.Config
	if cfg.Token != d.USDC || cfg.User != os.User || cfg.Shopper != os.Shopper || cfg.T1 != os.T1 || cfg.T2 != os.T2 {
		return Mismatchf("module config of %s does not match the quote", safe.Hex())
	}
	return nil
}

// VerifySafeFunding checks §4.6 for USDC: the Safe at the predicted address has the owners, threshold and
// module registration of the quote, fund_tx is a confirmed USDC transfer into it, it holds the lock amount, and
// fee_tx is a confirmed transfer from the user to the escrow.
func VerifySafeFunding(ctx context.Context, c *evm.Client, d *evm.Deployments, os evm.OrderSafe, orderID string, req *proto.OrderRequest, q *proto.OrderQuote, f *proto.OrderFunded, minConf int64) (Funding, error) {
	var out Funding
	if f == nil || f.Asset != proto.AssetUSDC || !common.IsHexAddress(f.Safe) {
		return out, Mismatchf("funded needs the safe address")
	}
	if f.FundTx == "" {
		return out, Mismatchf("funded needs fund_tx")
	}
	safe := common.HexToAddress(f.Safe)
	if err := CheckSafe(ctx, c, d, os, orderID, q, safe); err != nil {
		return out, err
	}
	lock, err := amount.ParseInt(q.LockAmount)
	if err != nil {
		return out, Definite(err)
	}
	// the confirmations are counted on a real transfer into the Safe, not on any transaction the user names
	fund, err := confirmedTransfers(ctx, c, d.USDC, f.FundTx, minConf)
	if err != nil {
		return out, err
	}
	// the named transaction itself must carry the lock: a small early transfer topped up after the quote
	// expired would otherwise date the funding before the deadline
	into := new(big.Int)
	for _, t := range fund.transfers {
		if t.To == safe {
			into.Add(into, t.Amount)
		}
	}
	if into.Sign() == 0 {
		return out, Mismatchf("fund_tx %s does not transfer USDC to the safe", f.FundTx)
	}
	if into.Cmp(lock) < 0 {
		return out, Mismatchf("fund_tx %s transfers %s to the safe, the lock is %s", f.FundTx, into, lock)
	}
	bal, err := c.BalanceOf(ctx, d.USDC, safe)
	if err != nil {
		return out, err
	}
	if bal.Cmp(lock) < 0 {
		return out, ErrNotYet
	}
	feeKey, err := VerifyUSDCFee(ctx, c, d, req, q, f, minConf)
	if err != nil {
		return out, err
	}
	out = Funding{Safe: safe, ConfirmedAt: fund.blockTime, Uses: []string{"safe:" + strings.ToLower(safe.Hex())}}
	if feeKey != "" {
		out.Uses = append(out.Uses, feeKey)
	}
	return out, nil
}

type minedTransfers struct {
	transfers []evm.TransferEvent
	blockTime int64
}

// confirmedTransfers reads the token transfers of a mined, successful transaction with minConf confirmations.
func confirmedTransfers(ctx context.Context, c *evm.Client, token common.Address, txHash string, minConf int64) (*minedTransfers, error) {
	if !isHash(txHash) {
		return nil, Mismatchf("%q is not a transaction hash", txHash)
	}
	r, err := c.Eth.TransactionReceipt(ctx, common.HexToHash(txHash))
	if errors.Is(err, ethereum.NotFound) {
		return nil, ErrNotYet
	}
	if err != nil {
		return nil, err
	}
	if r.Status != types.ReceiptStatusSuccessful {
		return nil, Mismatchf("transaction %s reverted", txHash)
	}
	head, err := c.Eth.BlockNumber(ctx)
	if err != nil {
		return nil, err
	}
	if minConf < 1 {
		minConf = 1
	}
	if int64(head)-r.BlockNumber.Int64()+1 < minConf {
		return nil, ErrNotYet
	}
	h, err := c.Eth.HeaderByNumber(ctx, r.BlockNumber)
	if err != nil {
		return nil, err
	}
	return &minedTransfers{transfers: evm.TransfersOf(r, token), blockTime: int64(h.Time)}, nil
}

func isHash(s string) bool { return IsTxHash(s) }

// VerifyUSDCFee checks that fee_tx is a confirmed USDC transfer of the upfront fee from user_evm_address to the
// escrow. It returns the key of the fee for the one-order-per-fee check ("" when no fee is due).
func VerifyUSDCFee(ctx context.Context, c *evm.Client, d *evm.Deployments, req *proto.OrderRequest, q *proto.OrderQuote, f *proto.OrderFunded, minConf int64) (string, error) {
	fee, err := amount.ParseInt(q.EscrowUpfrontFee)
	if err != nil {
		return "", Mismatchf("escrow_upfront_fee: %v", err)
	}
	if fee.Sign() == 0 {
		return "", nil
	}
	if f.FeeTx == "" {
		return "", Mismatchf("funded without fee_tx")
	}
	if !common.IsHexAddress(req.UserEVMAddress) || !common.IsHexAddress(q.EscrowEVMAddress) {
		return "", Mismatchf("user or escrow EVM address missing")
	}
	mined, err := confirmedTransfers(ctx, c, d.USDC, f.FeeTx, minConf)
	if err != nil {
		return "", err
	}
	from, to := common.HexToAddress(req.UserEVMAddress), common.HexToAddress(q.EscrowEVMAddress)
	paid := new(big.Int)
	for _, t := range mined.transfers {
		if t.From == from && t.To == to {
			paid.Add(paid, t.Amount)
		}
	}
	if paid.Cmp(fee) < 0 {
		return "", Mismatchf("escrow upfront fee: %s paid from %s to %s, %s due", paid, from.Hex(), to.Hex(), fee)
	}
	return "usdc-fee:" + strings.ToLower(f.FeeTx), nil
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

// SafeSettlement tells whether the Safe of an order was paid out (§4.8): its USDC balance is below the lock and
// a successful transaction moved USDC out of it. hint is a transaction a party named (may be ""); without it
// the Transfer logs of the Safe are searched. It returns the paying transaction ("" when not settled).
func SafeSettlement(ctx context.Context, c *evm.Client, d *evm.Deployments, safe common.Address, lock *big.Int, hint string) (string, error) {
	bal, err := c.BalanceOf(ctx, d.USDC, safe)
	if err != nil {
		return "", err
	}
	if bal.Cmp(lock) >= 0 {
		return "", nil
	}
	if IsTxHash(strings.ToLower(hint)) {
		if r, err := c.Receipt(ctx, common.HexToHash(hint)); err == nil && r.Status == types.ReceiptStatusSuccessful {
			for _, t := range evm.TransfersOf(r, d.USDC) {
				if t.From == safe {
					return strings.ToLower(hint), nil
				}
			}
		}
	}
	hash, err := c.LastTransferFrom(ctx, d.USDC, safe)
	if err != nil {
		return "", err
	}
	if hash == (common.Hash{}) {
		return "", nil
	}
	return strings.ToLower(hash.Hex()), nil
}
