package btc

import (
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"

	"github.com/pad01g/proxy-shopping-go/node/internal/keys"
)

type parties struct {
	u, s, e *btcec.PrivateKey
	esc     Escrow
}

func newParties(t *testing.T) parties {
	t.Helper()
	gen := func() *btcec.PrivateKey {
		k, err := btcec.NewPrivateKey()
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	p := parties{u: gen(), s: gen(), e: gen()}
	p.esc = Escrow{User: p.u.PubKey(), Shopper: p.s.PubKey(), Escrow: p.e.PubKey(), T1: 1100, T2: 1150}
	return p
}

var prev = Outpoint{TxID: strings.Repeat("ab", 32), Vout: 1, Amount: 90600}

// execute runs the script interpreter on input 0, as a full node would.
func execute(t *testing.T, tx *wire.MsgTx, esc Escrow) error {
	t.Helper()
	addr, _ := esc.Address()
	pk, _ := PkScript(addr)
	fetcher := txscript.NewCannedPrevOutputFetcher(pk, prev.Amount)
	vm, err := txscript.NewEngine(pk, tx, 0, txscript.StandardVerifyFlags, nil, txscript.NewTxSigHashes(tx, fetcher), prev.Amount, fetcher)
	if err != nil {
		return err
	}
	return vm.Execute()
}

func spend(t *testing.T, p parties, path Path, signers ...*btcec.PrivateKey) (*psbt.Packet, *wire.MsgTx, error) {
	t.Helper()
	outs := []Output{{Address: keys.P2WPKHAddress(p.s.PubKey()), Amount: 89600}}
	pk, err := p.esc.NewSpend(prev, outs, path)
	if err != nil {
		t.Fatal(err)
	}
	// round trip through base64 like the messages do
	b64, err := EncodePSBT(pk)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range signers {
		pk, err = DecodePSBT(b64)
		if err != nil {
			t.Fatal(err)
		}
		if err := Sign(pk, k); err != nil {
			t.Fatal(err)
		}
		if b64, err = EncodePSBT(pk); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := VerifySignatures(pk); err != nil {
		t.Fatal(err)
	}
	tx, err := p.esc.Finalize(pk, path)
	return pk, tx, err
}

func TestSpendPaths(t *testing.T) {
	p := newParties(t)
	cases := []struct {
		name    string
		path    Path
		signers []*btcec.PrivateKey
	}{
		{"user+shopper", PathMultisig, []*btcec.PrivateKey{p.u, p.s}},
		{"escrow+user", PathMultisig, []*btcec.PrivateKey{p.e, p.u}},
		{"shopper+escrow", PathMultisig, []*btcec.PrivateKey{p.s, p.e}},
		{"shopper after T1", PathShopperT1, []*btcec.PrivateKey{p.s}},
		{"user after T2", PathUserT2, []*btcec.PrivateKey{p.u}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, tx, err := spend(t, p, c.path, c.signers...)
			if err != nil {
				t.Fatal(err)
			}
			if err := execute(t, tx, p.esc); err != nil {
				t.Fatalf("script rejected: %v", err)
			}
			switch c.path {
			case PathShopperT1:
				if tx.LockTime != 1100 || tx.TxIn[0].Sequence != 0xfffffffe {
					t.Fatal("T1 locktime/sequence")
				}
			case PathMultisig:
				if tx.LockTime != 0 || tx.TxIn[0].Sequence != 0xffffffff {
					t.Fatal("multisig locktime/sequence")
				}
				// the size estimate used for the relay fee check is not below the real size
				script, _ := p.esc.Script()
				real := (int64(tx.SerializeSizeStripped())*3 + int64(tx.SerializeSize()) + 3) / 4
				if est := MultisigVSize(tx, script); est < real || est > real+2 {
					t.Fatalf("vsize estimate %d, real %d", est, real)
				}
			}
		})
	}
}

func TestSpendRejections(t *testing.T) {
	p := newParties(t)
	if _, _, err := spend(t, p, PathMultisig, p.u); err == nil {
		t.Fatal("one signature finalized a 2-of-3")
	}
	if _, _, err := spend(t, p, PathShopperT1, p.u); err == nil {
		t.Fatal("user signature finalized the shopper branch")
	}
	// a T1 spend with an earlier locktime is invalid on chain
	pk, _ := p.esc.NewSpend(prev, []Output{{Address: keys.P2WPKHAddress(p.s.PubKey()), Amount: 89600}}, PathShopperT1)
	pk.UnsignedTx.LockTime = 1099
	_ = Sign(pk, p.s)
	tx, err := p.esc.Finalize(pk, PathShopperT1)
	if err != nil {
		t.Fatal(err)
	}
	if err := execute(t, tx, p.esc); err == nil {
		t.Fatal("CLTV accepted a locktime below T1")
	}
	// the shopper cannot use the user's T2 branch
	pk, _ = p.esc.NewSpend(prev, []Output{{Address: keys.P2WPKHAddress(p.s.PubKey()), Amount: 89600}}, PathUserT2)
	_ = Sign(pk, p.s)
	pk.Inputs[0].PartialSigs[0].PubKey = p.u.PubKey().SerializeCompressed()
	if _, err := VerifySignatures(pk); err == nil {
		t.Fatal("forged partial signature verified")
	}
	if _, err := p.esc.NewSpend(prev, []Output{{Address: keys.P2WPKHAddress(p.s.PubKey()), Amount: prev.Amount + 1}}, PathMultisig); err == nil {
		t.Fatal("outputs above the input accepted")
	}
	outsider, _ := btcec.NewPrivateKey()
	pk, _ = p.esc.NewSpend(prev, []Output{{Address: keys.P2WPKHAddress(p.s.PubKey()), Amount: 1000}}, PathMultisig)
	if err := Sign(pk, outsider); err == nil {
		t.Fatal("outsider key signed")
	}
}

func TestScriptChecksAndOutputs(t *testing.T) {
	p := newParties(t)
	bad := p.esc
	bad.T2 = bad.T1
	if _, err := bad.Script(); err == nil {
		t.Fatal("t1 == t2 accepted")
	}
	addr, err := p.esc.Address()
	if err != nil || !strings.HasPrefix(addr, "tb1q") || len(addr) != 62 {
		t.Fatalf("address %q %v", addr, err)
	}
	if _, err := PkScript("bc1qar0srrr7xfkvy5l643lydnw9re59gtzzwf5mdq"); err == nil {
		t.Fatal("mainnet address accepted")
	}
	pk, _ := p.esc.NewSpend(prev, []Output{{Address: addr, Amount: 5000}, {Address: keys.P2WPKHAddress(p.u.PubKey()), Amount: 0}}, PathMultisig)
	outs, err := Outputs(pk)
	if err != nil || len(outs) != 1 || outs[0].Address != addr || outs[0].Amount != 5000 {
		t.Fatalf("outputs %+v %v", outs, err)
	}
	// dust outputs are refused, not created (the transaction would not relay)
	if _, err := p.esc.NewSpend(prev, []Output{{Address: addr, Amount: DustLimit - 1}}, PathMultisig); err == nil {
		t.Fatal("dust output created")
	}
	// the size estimate of a 2-of-3 payout with three outputs: about 230 vbytes
	pk, _ = p.esc.NewSpend(prev, []Output{{Address: addr, Amount: 5000}, {Address: keys.P2WPKHAddress(p.u.PubKey()), Amount: 5000},
		{Address: keys.P2WPKHAddress(p.s.PubKey()), Amount: 5000}}, PathMultisig)
	script, _ := p.esc.Script()
	if n := MultisigVSize(pk.UnsignedTx, script); n < 200 || n > 280 {
		t.Fatalf("vsize %d", n)
	}
}
