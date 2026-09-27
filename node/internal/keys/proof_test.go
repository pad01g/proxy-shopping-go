package keys

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestKeyProof(t *testing.T) {
	const order = "000102030405060708090a0b0c0d0e0f"
	s, err := FromMnemonic("abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about")
	if err != nil {
		t.Fatal(err)
	}
	other, _ := FromMnemonic("zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo zoo wrong")
	nostrPub := s.NostrPubHex()
	if m := KeyProofMessage(order, nostrPub); m != "ps-key-proof-v1|"+order+"|"+nostrPub {
		t.Fatal(m)
	}

	// BTC: every order key, whatever the parity of its y coordinate, proves itself with its x-only key
	for _, id := range []string{order, "ffffffffffffffffffffffffffffffff", "0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"} {
		k, _ := s.OrderKey(id)
		pub := hex.EncodeToString(k.PubKey().SerializeCompressed())
		proof, err := SignKeyProofBTC(k, id, nostrPub)
		if err != nil {
			t.Fatal(err)
		}
		if again, _ := SignKeyProofBTC(k, id, nostrPub); again != proof {
			t.Fatal("BTC key proof is not deterministic")
		}
		if err := VerifyKeyProofBTC(pub, id, nostrPub, proof); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if VerifyKeyProofBTC(pub, id, other.NostrPubHex(), proof) == nil {
			t.Fatal("proof for another identity accepted")
		}
		if VerifyKeyProofBTC(pub, "11111111111111111111111111111111", nostrPub, proof) == nil {
			t.Fatal("proof for another order accepted")
		}
		ok, _ := other.OrderKey(id)
		if VerifyKeyProofBTC(hex.EncodeToString(ok.PubKey().SerializeCompressed()), id, nostrPub, proof) == nil {
			t.Fatal("proof accepted for another chain key")
		}
		if VerifyKeyProofBTC(pub, id, nostrPub, strings.ToUpper(proof)) == nil || VerifyKeyProofBTC(pub, id, nostrPub, proof[:126]) == nil {
			t.Fatal("malformed proof accepted")
		}
	}

	// EVM: personal_sign by the account, with or without 0x
	proof, err := SignKeyProofEVM(s.EVM, order, nostrPub)
	if err != nil || len(proof) != 130 {
		t.Fatalf("%s %v", proof, err)
	}
	if err := VerifyKeyProofEVM(s.EVMAddress().Hex(), order, nostrPub, proof); err != nil {
		t.Fatal(err)
	}
	if err := VerifyKeyProofEVM(strings.ToLower(s.EVMAddress().Hex()), order, nostrPub, "0x"+proof); err != nil {
		t.Fatal(err)
	}
	if VerifyKeyProofEVM(other.EVMAddress().Hex(), order, nostrPub, proof) == nil {
		t.Fatal("proof accepted for another account")
	}
	if VerifyKeyProofEVM(s.EVMAddress().Hex(), order, other.NostrPubHex(), proof) == nil {
		t.Fatal("proof accepted for another identity")
	}
	raw := []byte(proof)
	raw[128], raw[129] = '0', '1' // v = 1 instead of 27/28
	if VerifyKeyProofEVM(s.EVMAddress().Hex(), order, nostrPub, string(raw)) == nil {
		t.Fatal("v = 1 accepted")
	}
}
