// Package delivery encrypts the delivery address of an order (spec §4.4): the address is sealed with a random
// key K (XChaCha20-Poly1305, aad = order id), and K is given to the shopper and the escrow with NIP-44.
package delivery

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nbd-wtf/go-nostr/nip44"
	"golang.org/x/crypto/chacha20poly1305"

	"github.com/pad01g/proxy-shopping-go/node/internal/keys"
)

// Address is the shipping address.
type Address struct {
	Name       string `json:"name"`
	PostalCode string `json:"postal_code"`
	Address    string `json:"address"`
	Phone      string `json:"phone"`
}

// NewKey returns a random 32 byte K.
func NewKey() ([32]byte, error) {
	var k [32]byte
	_, err := rand.Read(k[:])
	return k, err
}

// Seal encrypts addr under K; a nil nonce draws a random one.
func Seal(k [32]byte, nonce []byte, orderID string, addr Address) (string, error) {
	aad, err := keys.OrderIDBytes(orderID)
	if err != nil {
		return "", err
	}
	aead, err := chacha20poly1305.NewX(k[:])
	if err != nil {
		return "", err
	}
	if nonce == nil {
		nonce = make([]byte, chacha20poly1305.NonceSizeX)
		if _, err := rand.Read(nonce); err != nil {
			return "", err
		}
	}
	if len(nonce) != chacha20poly1305.NonceSizeX {
		return "", errors.New("nonce must be 24 bytes")
	}
	plain, err := json.Marshal(addr)
	if err != nil {
		return "", err
	}
	out := aead.Seal(append([]byte{}, nonce...), nonce, plain, aad)
	return base64.StdEncoding.EncodeToString(out), nil
}

// Open decrypts a ciphertext of Seal.
func Open(k [32]byte, ciphertext, orderID string) (Address, error) {
	var addr Address
	aad, err := keys.OrderIDBytes(orderID)
	if err != nil {
		return addr, err
	}
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return addr, fmt.Errorf("delivery ciphertext: %w", err)
	}
	if len(raw) < chacha20poly1305.NonceSizeX+chacha20poly1305.Overhead {
		return addr, errors.New("delivery ciphertext too short")
	}
	aead, err := chacha20poly1305.NewX(k[:])
	if err != nil {
		return addr, err
	}
	plain, err := aead.Open(nil, raw[:chacha20poly1305.NonceSizeX], raw[chacha20poly1305.NonceSizeX:], aad)
	if err != nil {
		return addr, errors.New("delivery ciphertext does not decrypt with this key")
	}
	if err := json.Unmarshal(plain, &addr); err != nil {
		return addr, fmt.Errorf("delivery address: %w", err)
	}
	return addr, nil
}

// WrapKey encrypts hex(K) for a recipient with NIP-44 v2 (key_for_shopper / key_for_escrow).
func WrapKey(senderSecretHex, recipientPubHex string, k [32]byte) (string, error) {
	ck, err := nip44.GenerateConversationKey(recipientPubHex, senderSecretHex)
	if err != nil {
		return "", fmt.Errorf("conversation key: %w", err)
	}
	return nip44.Encrypt(hex.EncodeToString(k[:]), ck)
}

// UnwrapKey decrypts a key of WrapKey; the recipient uses its secret and the sender's (user's) public key.
func UnwrapKey(recipientSecretHex, senderPubHex, wrapped string) ([32]byte, error) {
	var k [32]byte
	ck, err := nip44.GenerateConversationKey(senderPubHex, recipientSecretHex)
	if err != nil {
		return k, fmt.Errorf("conversation key: %w", err)
	}
	plain, err := nip44.Decrypt(wrapped, ck)
	if err != nil {
		return k, fmt.Errorf("decrypt delivery key: %w", err)
	}
	raw, err := hex.DecodeString(plain)
	if err != nil || len(raw) != 32 {
		return k, errors.New("delivery key is not 32 bytes of hex")
	}
	copy(k[:], raw)
	return k, nil
}
