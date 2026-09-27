package keys

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// KeyProofMessage is the message m of spec §4.4.1 that ties the user's Nostr identity to the chain key of an order.
func KeyProofMessage(orderID, userNostrPubHex string) string {
	return "ps-key-proof-v1|" + orderID + "|" + userNostrPubHex
}

// SignKeyProofBTC is the BIP340 signature over SHA-256(m) with the user's BTC order key. The auxiliary randomness
// is 32 zero bytes, so the proof is reproducible (any valid BIP340 signature verifies).
func SignKeyProofBTC(key *btcec.PrivateKey, orderID, userNostrPubHex string) (string, error) {
	h := sha256.Sum256([]byte(KeyProofMessage(orderID, userNostrPubHex)))
	sig, err := schnorr.Sign(key, h[:], schnorr.CustomNonce([32]byte{}))
	if err != nil {
		return "", fmt.Errorf("key proof: %w", err)
	}
	return hex.EncodeToString(sig.Serialize()), nil
}

// VerifyKeyProofBTC checks a BIP340 key proof against the x-only form of the 33 byte user_btc_pubkey.
func VerifyKeyProofBTC(userBTCPubHex, orderID, userNostrPubHex, proofHex string) error {
	pub, err := ParsePubKeyHex(userBTCPubHex)
	if err != nil {
		return err
	}
	raw, err := hex.DecodeString(proofHex)
	if err != nil || len(raw) != schnorr.SignatureSize || strings.ToLower(proofHex) != proofHex {
		return errors.New("key_proof must be 64 bytes of lower case hex")
	}
	sig, err := schnorr.ParseSignature(raw)
	if err != nil {
		return fmt.Errorf("key_proof: %w", err)
	}
	xonly, err := schnorr.ParsePubKey(schnorr.SerializePubKey(pub))
	if err != nil {
		return err
	}
	h := sha256.Sum256([]byte(KeyProofMessage(orderID, userNostrPubHex)))
	if !sig.Verify(h[:], xonly) {
		return errors.New("key_proof does not verify with user_btc_pubkey")
	}
	return nil
}

// SignKeyProofEVM is the EIP-191 personal_sign of m by the EVM account (65 bytes, v = 27/28).
func SignKeyProofEVM(key *btcec.PrivateKey, orderID, userNostrPubHex string) (string, error) {
	sig, err := crypto.Sign(accounts.TextHash([]byte(KeyProofMessage(orderID, userNostrPubHex))), key.ToECDSA())
	if err != nil {
		return "", fmt.Errorf("key proof: %w", err)
	}
	sig[64] += 27
	return hex.EncodeToString(sig), nil
}

// VerifyKeyProofEVM checks that a personal_sign key proof was made by user_evm_address. A 0x prefix is accepted.
func VerifyKeyProofEVM(userEVMAddress, orderID, userNostrPubHex, proofHex string) error {
	if !common.IsHexAddress(userEVMAddress) {
		return errors.New("user_evm_address is not an address")
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(proofHex, "0x"))
	if err != nil || len(raw) != 65 {
		return errors.New("key_proof must be 65 bytes of hex")
	}
	if raw[64] != 27 && raw[64] != 28 {
		return errors.New("key_proof must have v = 27 or 28")
	}
	sig := append([]byte{}, raw...)
	sig[64] -= 27
	pub, err := crypto.SigToPub(accounts.TextHash([]byte(KeyProofMessage(orderID, userNostrPubHex))), sig)
	if err != nil {
		return fmt.Errorf("key_proof: %w", err)
	}
	if crypto.PubkeyToAddress(*pub) != common.HexToAddress(userEVMAddress) {
		return errors.New("key_proof is not signed by user_evm_address")
	}
	return nil
}
