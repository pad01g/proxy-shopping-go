package contract

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/ethereum/go-ethereum/common"

	"github.com/pad01g/proxy-shopping-go/node/internal/btc"
	"github.com/pad01g/proxy-shopping-go/node/internal/evm"
)

// CheckBTCPayout verifies a 2-of-3 payout PSBT: it spends exactly the funded output with our script, pays at
// least `want` to each listed address and nothing else except `extra` (address → maximum), leaves at most
// maxFee as fee, and carries a valid signature of signedBy.
func CheckBTCPayout(p *psbt.Packet, esc btc.Escrow, prev btc.Outpoint, want map[string]int64, extra map[string]int64, maxFee int64, signedBy *btcec.PublicKey) error {
	script, err := esc.Script()
	if err != nil {
		return err
	}
	tx := p.UnsignedTx
	if len(tx.TxIn) != 1 || len(p.Inputs) != 1 {
		return errors.New("payout must have exactly one input")
	}
	in := tx.TxIn[0]
	if in.PreviousOutPoint.Hash.String() != prev.TxID || in.PreviousOutPoint.Index != prev.Vout {
		return fmt.Errorf("payout spends %s, not the escrow output %s:%d", in.PreviousOutPoint, prev.TxID, prev.Vout)
	}
	pin := p.Inputs[0]
	if !bytes.Equal(pin.WitnessScript, script) || pin.WitnessUtxo == nil || pin.WitnessUtxo.Value != prev.Amount {
		return errors.New("payout input does not describe the escrow output")
	}
	if tx.LockTime != 0 {
		return errors.New("2-of-3 payout must not be time locked")
	}
	outs, err := btc.Outputs(p)
	if err != nil {
		return err
	}
	got := map[string]int64{}
	var total int64
	for _, o := range outs {
		if o.Amount < btc.DustLimit {
			return fmt.Errorf("payout has a dust output of %d sats to %s: it would not relay", o.Amount, o.Address)
		}
		got[o.Address] += o.Amount
		total += o.Amount
	}
	for addr, amt := range want {
		if got[addr] < amt {
			return fmt.Errorf("payout gives %s %d sats, %d expected", addr, got[addr], amt)
		}
	}
	for addr, amt := range got {
		if _, ok := want[addr]; ok {
			continue
		}
		if max, ok := extra[addr]; !ok || amt > max {
			return fmt.Errorf("payout has an unexpected output to %s (%d sats)", addr, amt)
		}
	}
	if fee := prev.Amount - total; fee < 0 || fee > maxFee {
		return fmt.Errorf("payout fee %d exceeds %d", prev.Amount-total, maxFee)
	}
	signers, err := btc.VerifySignatures(p)
	if err != nil {
		return err
	}
	if signedBy != nil {
		for _, s := range signers {
			if bytes.Equal(s, signedBy.SerializeCompressed()) {
				return nil
			}
		}
		return errors.New("payout lacks the expected partial signature")
	}
	return nil
}

// CheckSafePayout verifies a SafeTx: it pays exactly the listed ERC-20 transfers (address → amount), has the
// expected nonce and no gas refund, and sig is a valid signature of signer over its EIP-712 hash.
func CheckSafePayout(t evm.SafeTx, d *evm.Deployments, chainID int64, safe common.Address, nonce *big.Int, want map[common.Address]*big.Int, sig []byte, signer common.Address) error {
	transfers, err := evm.DecodeTransfers(t, d.USDC, d.Safe.MultiSendCallOnly)
	if err != nil {
		return err
	}
	got := map[common.Address]*big.Int{}
	for _, tr := range transfers {
		if got[tr.To] == nil {
			got[tr.To] = new(big.Int)
		}
		got[tr.To].Add(got[tr.To], tr.Amount)
	}
	for addr, amt := range want {
		if amt.Sign() == 0 {
			continue
		}
		if got[addr] == nil || got[addr].Cmp(amt) != 0 {
			return fmt.Errorf("safe tx pays %s %v, %s expected", addr.Hex(), got[addr], amt)
		}
	}
	for addr := range got {
		if w, ok := want[addr]; !ok || w.Sign() == 0 {
			return fmt.Errorf("safe tx pays unexpected %s", addr.Hex())
		}
	}
	zero := func(b *big.Int) bool { return b == nil || b.Sign() == 0 }
	if !zero(t.SafeTxGas) || !zero(t.BaseGas) || !zero(t.GasPrice) || t.GasToken != (common.Address{}) || t.RefundReceiver != (common.Address{}) {
		return errors.New("safe tx must not refund gas")
	}
	if t.Nonce == nil || t.Nonce.Cmp(nonce) != 0 {
		return fmt.Errorf("safe tx nonce %v, safe is at %v", t.Nonce, nonce)
	}
	who, err := evm.Recover(t.Hash(chainID, safe), sig)
	if err != nil {
		return err
	}
	if who != signer {
		return fmt.Errorf("safe tx signed by %s, not %s", who.Hex(), signer.Hex())
	}
	return nil
}

// ParseSig decodes a 0x hex signature.
func ParseSig(s string) ([]byte, error) {
	b := common.FromHex(s)
	if len(b) != 65 || !strings.HasPrefix(s, "0x") {
		return nil, errors.New("signature must be 65 bytes of 0x hex")
	}
	return b, nil
}
