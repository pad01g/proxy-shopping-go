// Package btc implements the P2WSH escrow of spec §5: the witness script, its address and the PSBTs of the
// three spending paths.
package btc

import (
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"

	"github.com/pad01g/proxy-shopping-go/node/internal/keys"
)

// maxHeightLock is the boundary between block heights and timestamps of nLockTime.
const maxHeightLock = 500000000

// Escrow is the 2-of-3 contract of one order. The key order U, S, E is fixed.
type Escrow struct {
	User, Shopper, Escrow *btcec.PublicKey
	T1, T2                uint32 // block heights
}

// Script returns the witness script of §5.1.
func (e Escrow) Script() ([]byte, error) {
	if e.User == nil || e.Shopper == nil || e.Escrow == nil {
		return nil, errors.New("escrow needs three keys")
	}
	if e.T1 == 0 || e.T1 >= e.T2 || e.T2 >= maxHeightLock {
		return nil, fmt.Errorf("invalid timelocks t1=%d t2=%d", e.T1, e.T2)
	}
	u, s, x := e.User.SerializeCompressed(), e.Shopper.SerializeCompressed(), e.Escrow.SerializeCompressed()
	return txscript.NewScriptBuilder().
		AddOp(txscript.OP_IF).
		AddOp(txscript.OP_2).AddData(u).AddData(s).AddData(x).AddOp(txscript.OP_3).AddOp(txscript.OP_CHECKMULTISIG).
		AddOp(txscript.OP_ELSE).
		AddOp(txscript.OP_IF).
		AddInt64(int64(e.T1)).AddOp(txscript.OP_CHECKLOCKTIMEVERIFY).AddOp(txscript.OP_DROP).AddData(s).AddOp(txscript.OP_CHECKSIG).
		AddOp(txscript.OP_ELSE).
		AddInt64(int64(e.T2)).AddOp(txscript.OP_CHECKLOCKTIMEVERIFY).AddOp(txscript.OP_DROP).AddData(u).AddOp(txscript.OP_CHECKSIG).
		AddOp(txscript.OP_ENDIF).
		AddOp(txscript.OP_ENDIF).
		Script()
}

// Address returns the P2WSH address of the script (tb1q…).
func (e Escrow) Address() (string, error) {
	script, err := e.Script()
	if err != nil {
		return "", err
	}
	return ScriptAddress(script)
}

// ScriptAddress is the P2WSH address of a witness script.
func ScriptAddress(script []byte) (string, error) {
	h := sha256.Sum256(script)
	addr, err := btcutil.NewAddressWitnessScriptHash(h[:], keys.BTCParams)
	if err != nil {
		return "", err
	}
	return addr.EncodeAddress(), nil
}

// PkScript returns the output script paying to an address.
func PkScript(address string) ([]byte, error) {
	addr, err := btcutil.DecodeAddress(address, keys.BTCParams)
	if err != nil {
		return nil, fmt.Errorf("decode address %q: %w", address, err)
	}
	if !addr.IsForNet(keys.BTCParams) {
		return nil, fmt.Errorf("address %q is not a signet address", address)
	}
	return txscript.PayToAddrScript(addr)
}

// paramsForScripts decodes output scripts back to addresses.
var paramsForScripts = keys.BTCParams
