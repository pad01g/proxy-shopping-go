package evm

import (
	"bytes"
	"encoding/json"
	"math/big"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

func addr(n byte) common.Address {
	var a common.Address
	a[19] = n
	return a
}

func TestSafeTxJSONRoundTrip(t *testing.T) {
	tx, err := SplitTx(addr(7), addr(1), []Transfer{{To: addr(2), Amount: big.NewInt(5)}, {To: addr(3), Amount: big.NewInt(0)}}, big.NewInt(3))
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(tx)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	for _, k := range []string{"value", "operation", "safeTxGas", "baseGas", "gasPrice", "nonce"} {
		if _, ok := m[k].(string); !ok {
			t.Fatalf("%s is not a decimal string: %s", k, data)
		}
	}
	var back SafeTx
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Hash(31337, addr(9)) != tx.Hash(31337, addr(9)) {
		t.Fatal("hash changed by the JSON round trip")
	}
	// operation may also be a number
	var num SafeTx
	if err := json.Unmarshal([]byte(`{"to":"0x0000000000000000000000000000000000000001","value":"0","data":"0x","operation":1,"nonce":"0"}`), &num); err != nil || num.Operation != 1 {
		t.Fatalf("numeric operation: %v", err)
	}
	if err := json.Unmarshal([]byte(`{"operation":"2"}`), &num); err == nil {
		t.Fatal("operation 2 accepted")
	}
}

func TestSignRecoverJoin(t *testing.T) {
	h := crypto.Keccak256Hash([]byte("x"))
	var keys []*btcec.PrivateKey
	sigs := map[common.Address][]byte{}
	for i := 0; i < 3; i++ {
		k, _ := btcec.NewPrivateKey()
		keys = append(keys, k)
		sig, err := Sign(h, k)
		if err != nil {
			t.Fatal(err)
		}
		if sig[64] != 27 && sig[64] != 28 {
			t.Fatal("v must be 27/28")
		}
		who, err := Recover(h, sig)
		if err != nil || who != crypto.PubkeyToAddress(*k.PubKey().ToECDSA()) {
			t.Fatal("recover")
		}
		sigs[who] = sig
	}
	joined := JoinSignatures(sigs)
	var prev common.Address
	for i := 0; i < 3; i++ {
		who, _ := Recover(h, joined[i*65:(i+1)*65])
		if i > 0 && bytes.Compare(prev.Bytes(), who.Bytes()) >= 0 {
			t.Fatal("signatures not sorted by owner")
		}
		prev = who
	}
}

func TestDecodeTransfers(t *testing.T) {
	token, ms := addr(1), addr(7)
	rel, _ := ReleaseTx(token, addr(2), big.NewInt(10), nil)
	got, err := DecodeTransfers(rel, token, ms)
	if err != nil || len(got) != 1 || got[0].To != addr(2) || got[0].Amount.Int64() != 10 {
		t.Fatalf("release %+v %v", got, err)
	}
	split, _ := SplitTx(ms, token, []Transfer{{To: addr(2), Amount: big.NewInt(1)}, {To: addr(3), Amount: big.NewInt(2)}}, nil)
	got, err = DecodeTransfers(split, token, ms)
	if err != nil || len(got) != 2 || got[1].To != addr(3) || got[1].Amount.Int64() != 2 {
		t.Fatalf("split %+v %v", got, err)
	}
	other := rel
	other.To = addr(5)
	if _, err := DecodeTransfers(other, token, ms); err == nil {
		t.Fatal("transfer of another token accepted")
	}
	call := split
	call.Operation = 0
	if _, err := DecodeTransfers(call, token, ms); err == nil {
		t.Fatal("multiSend as call accepted")
	}
}

func TestInitializerRejectsBadTimelock(t *testing.T) {
	o := OrderSafe{User: addr(1), Shopper: addr(2), Escrow: addr(3), T1: 10, T2: 10}
	if _, err := o.Initializer(); err == nil {
		t.Fatal("t1 == t2 accepted")
	}
	if _, err := SaltNonce("xyz"); err == nil {
		t.Fatal("bad order id accepted")
	}
}
