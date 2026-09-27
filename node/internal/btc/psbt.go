package btc

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// Path is one of the three ways to spend the escrow output (§5.1).
type Path int

const (
	PathMultisig  Path = iota // 2-of-3, any time
	PathShopperT1             // shopper alone from T1
	PathUserT2                // user alone from T2
)

func (p Path) String() string {
	switch p {
	case PathMultisig:
		return "multisig"
	case PathShopperT1:
		return "shopper-t1"
	case PathUserT2:
		return "user-t2"
	}
	return fmt.Sprintf("path(%d)", int(p))
}

// Output is a payment of a spend.
type Output struct {
	Address string
	Amount  int64
}

// Outpoint identifies the funded escrow output.
type Outpoint struct {
	TxID   string
	Vout   uint32
	Amount int64
}

// NewSpend creates the unsigned PSBT that spends the escrow output along a path.
func (e Escrow) NewSpend(prev Outpoint, outs []Output, path Path) (*psbt.Packet, error) {
	script, err := e.Script()
	if err != nil {
		return nil, err
	}
	hash, err := chainhash.NewHashFromStr(prev.TxID)
	if err != nil {
		return nil, fmt.Errorf("funding txid: %w", err)
	}
	tx := wire.NewMsgTx(2)
	in := wire.NewTxIn(wire.NewOutPoint(hash, prev.Vout), nil, nil)
	switch path {
	case PathMultisig:
		in.Sequence = wire.MaxTxInSequenceNum
	case PathShopperT1:
		in.Sequence = wire.MaxTxInSequenceNum - 1
		tx.LockTime = e.T1
	case PathUserT2:
		in.Sequence = wire.MaxTxInSequenceNum - 1
		tx.LockTime = e.T2
	default:
		return nil, fmt.Errorf("unknown path %d", path)
	}
	tx.AddTxIn(in)
	var total int64
	for _, o := range outs {
		if o.Amount <= 0 {
			continue // zero outputs are never created (§5.2)
		}
		pk, err := PkScript(o.Address)
		if err != nil {
			return nil, err
		}
		tx.AddTxOut(wire.NewTxOut(o.Amount, pk))
		total += o.Amount
	}
	if len(tx.TxOut) == 0 {
		return nil, errors.New("spend without outputs")
	}
	if total > prev.Amount {
		return nil, fmt.Errorf("outputs %d exceed the escrow amount %d", total, prev.Amount)
	}
	p, err := psbt.NewFromUnsignedTx(tx)
	if err != nil {
		return nil, fmt.Errorf("new psbt: %w", err)
	}
	addr, err := ScriptAddress(script)
	if err != nil {
		return nil, err
	}
	pk, err := PkScript(addr)
	if err != nil {
		return nil, err
	}
	p.Inputs[0].WitnessUtxo = wire.NewTxOut(prev.Amount, pk)
	p.Inputs[0].WitnessScript = script
	p.Inputs[0].SighashType = txscript.SigHashAll
	return p, nil
}

// Sign adds a SIGHASH_ALL BIP143 signature of priv to every input whose witness script contains its key.
func Sign(p *psbt.Packet, priv *btcec.PrivateKey) error {
	pub := priv.PubKey().SerializeCompressed()
	fetcher := txscript.NewMultiPrevOutFetcher(nil)
	for i, in := range p.Inputs {
		if in.WitnessUtxo == nil {
			return fmt.Errorf("input %d has no witness utxo", i)
		}
		fetcher.AddPrevOut(p.UnsignedTx.TxIn[i].PreviousOutPoint, in.WitnessUtxo)
	}
	hashes := txscript.NewTxSigHashes(p.UnsignedTx, fetcher)
	signed := 0
	for i, in := range p.Inputs {
		if !bytes.Contains(in.WitnessScript, pub) {
			continue
		}
		digest, err := txscript.CalcWitnessSigHash(in.WitnessScript, hashes, txscript.SigHashAll, p.UnsignedTx, i, in.WitnessUtxo.Value)
		if err != nil {
			return fmt.Errorf("sighash of input %d: %w", i, err)
		}
		sig := append(ecdsa.Sign(priv, digest).Serialize(), byte(txscript.SigHashAll))
		p.Inputs[i].PartialSigs = append(withoutSigOf(in.PartialSigs, pub), &psbt.PartialSig{PubKey: pub, Signature: sig})
		signed++
	}
	if signed == 0 {
		return errors.New("the key is not part of any input script")
	}
	return nil
}

func withoutSigOf(sigs []*psbt.PartialSig, pub []byte) []*psbt.PartialSig {
	out := sigs[:0:0]
	for _, s := range sigs {
		if !bytes.Equal(s.PubKey, pub) {
			out = append(out, s)
		}
	}
	return out
}

// VerifySignatures checks every partial signature of the PSBT and returns the keys that signed input 0.
func VerifySignatures(p *psbt.Packet) ([][]byte, error) {
	fetcher := txscript.NewMultiPrevOutFetcher(nil)
	for i, in := range p.Inputs {
		if in.WitnessUtxo == nil {
			return nil, fmt.Errorf("input %d has no witness utxo", i)
		}
		fetcher.AddPrevOut(p.UnsignedTx.TxIn[i].PreviousOutPoint, in.WitnessUtxo)
	}
	hashes := txscript.NewTxSigHashes(p.UnsignedTx, fetcher)
	var signers [][]byte
	for i, in := range p.Inputs {
		digest, err := txscript.CalcWitnessSigHash(in.WitnessScript, hashes, txscript.SigHashAll, p.UnsignedTx, i, in.WitnessUtxo.Value)
		if err != nil {
			return nil, err
		}
		for _, ps := range in.PartialSigs {
			if len(ps.Signature) < 2 || ps.Signature[len(ps.Signature)-1] != byte(txscript.SigHashAll) {
				return nil, fmt.Errorf("input %d: signature is not SIGHASH_ALL", i)
			}
			sig, err := ecdsa.ParseDERSignature(ps.Signature[:len(ps.Signature)-1])
			if err != nil {
				return nil, fmt.Errorf("input %d: %w", i, err)
			}
			pub, err := btcec.ParsePubKey(ps.PubKey)
			if err != nil {
				return nil, fmt.Errorf("input %d: %w", i, err)
			}
			if !sig.Verify(digest, pub) {
				return nil, fmt.Errorf("input %d: invalid signature of %x", i, ps.PubKey)
			}
			if i == 0 {
				signers = append(signers, ps.PubKey)
			}
		}
	}
	return signers, nil
}

// Finalize builds the witness of the path from the partial signatures and returns the signed transaction.
func (e Escrow) Finalize(p *psbt.Packet, path Path) (*wire.MsgTx, error) {
	script, err := e.Script()
	if err != nil {
		return nil, err
	}
	for i := range p.Inputs {
		in := &p.Inputs[i]
		if !bytes.Equal(in.WitnessScript, script) {
			return nil, fmt.Errorf("input %d does not spend this escrow", i)
		}
		sigOf := func(k *btcec.PublicKey) []byte {
			pub := k.SerializeCompressed()
			for _, s := range in.PartialSigs {
				if bytes.Equal(s.PubKey, pub) {
					return s.Signature
				}
			}
			return nil
		}
		var wit wire.TxWitness
		switch path {
		case PathMultisig:
			wit = wire.TxWitness{nil}
			for _, k := range []*btcec.PublicKey{e.User, e.Shopper, e.Escrow} {
				if s := sigOf(k); s != nil && len(wit) < 3 {
					wit = append(wit, s)
				}
			}
			if len(wit) != 3 {
				return nil, fmt.Errorf("input %d: need 2 of 3 signatures, have %d", i, len(wit)-1)
			}
			wit = append(wit, []byte{1}, script)
		case PathShopperT1:
			s := sigOf(e.Shopper)
			if s == nil {
				return nil, fmt.Errorf("input %d: no shopper signature", i)
			}
			wit = wire.TxWitness{s, []byte{1}, nil, script}
		case PathUserT2:
			s := sigOf(e.User)
			if s == nil {
				return nil, fmt.Errorf("input %d: no user signature", i)
			}
			wit = wire.TxWitness{s, nil, nil, script}
		default:
			return nil, fmt.Errorf("unknown path %d", path)
		}
		var buf bytes.Buffer
		if err := psbt.WriteTxWitness(&buf, wit); err != nil {
			return nil, err
		}
		in.FinalScriptWitness = buf.Bytes()
		in.PartialSigs, in.WitnessScript, in.SighashType = nil, nil, 0
	}
	tx, err := psbt.Extract(p)
	if err != nil {
		return nil, fmt.Errorf("extract: %w", err)
	}
	return tx, nil
}

// EncodePSBT returns the base64 form used in messages.
func EncodePSBT(p *psbt.Packet) (string, error) { return p.B64Encode() }

// DecodePSBT parses a base64 PSBT.
func DecodePSBT(s string) (*psbt.Packet, error) {
	p, err := psbt.NewFromRawBytes(strings.NewReader(s), true)
	if err != nil {
		return nil, fmt.Errorf("decode psbt: %w", err)
	}
	return p, nil
}

// TxHex serializes a signed transaction.
func TxHex(tx *wire.MsgTx) string {
	var buf bytes.Buffer
	_ = tx.Serialize(&buf)
	return fmt.Sprintf("%x", buf.Bytes())
}

// Outputs lists the payments of a PSBT as addresses and amounts.
func Outputs(p *psbt.Packet) ([]Output, error) {
	var outs []Output
	for _, o := range p.UnsignedTx.TxOut {
		_, addrs, _, err := txscript.ExtractPkScriptAddrs(o.PkScript, paramsForScripts)
		if err != nil || len(addrs) != 1 {
			return nil, fmt.Errorf("unsupported output script %x", o.PkScript)
		}
		outs = append(outs, Output{Address: addrs[0].EncodeAddress(), Amount: o.Value})
	}
	return outs, nil
}
