// Package keys derives every key a participant needs from one BIP39 mnemonic (spec §1).
package keys

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/tyler-smith/go-bip39"
)

const hardened = hdkeychain.HardenedKeyStart

// BTCParams are the address parameters of the lab signet (tb1… addresses).
var BTCParams = &chaincfg.SigNetParams

// Derivation paths of spec §1.
var (
	PathNostr   = []uint32{44 + hardened, 1237 + hardened, 0 + hardened, 0, 0}
	PathLibp2p  = []uint32{7333 + hardened, 0 + hardened, 0 + hardened}
	PathOrder   = []uint32{7333 + hardened, 1 + hardened} // + {idx}'
	PathEscrow  = []uint32{7333 + hardened, 2 + hardened} // + {idx}
	PathWallet  = []uint32{84 + hardened, 1 + hardened, 0 + hardened, 0, 0}
	PathEVM     = []uint32{44 + hardened, 60 + hardened, 0 + hardened, 0, 0}
	errBadOrder = errors.New("order id must be 16 bytes of hex")
)

// Set holds the keys of one participant.
type Set struct {
	master *hdkeychain.ExtendedKey

	Nostr  *btcec.PrivateKey
	Libp2p *btcec.PrivateKey
	Wallet *btcec.PrivateKey
	EVM    *btcec.PrivateKey
}

// LoadMnemonicFile reads a mnemonic file (whitespace is normalized) and derives the key set.
func LoadMnemonicFile(path string) (*Set, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read mnemonic: %w", err)
	}
	return FromMnemonic(string(data))
}

// FromMnemonic derives the key set from a BIP39 mnemonic without passphrase.
func FromMnemonic(mnemonic string) (*Set, error) {
	mnemonic = strings.Join(strings.Fields(mnemonic), " ")
	if !bip39.IsMnemonicValid(mnemonic) {
		return nil, errors.New("invalid BIP39 mnemonic")
	}
	seed := bip39.NewSeed(mnemonic, "")
	// testnet version bytes, so that the escrow xpub is a tpub
	master, err := hdkeychain.NewMaster(seed, &chaincfg.TestNet3Params)
	if err != nil {
		return nil, fmt.Errorf("master key: %w", err)
	}
	s := &Set{master: master}
	for _, d := range []struct {
		path []uint32
		dst  **btcec.PrivateKey
	}{
		{PathNostr, &s.Nostr}, {PathLibp2p, &s.Libp2p}, {PathWallet, &s.Wallet}, {PathEVM, &s.EVM},
	} {
		k, err := s.derivePriv(d.path...)
		if err != nil {
			return nil, err
		}
		*d.dst = k
	}
	return s, nil
}

func (s *Set) deriveExt(path ...uint32) (*hdkeychain.ExtendedKey, error) {
	k := s.master
	for _, i := range path {
		var err error
		if k, err = k.Derive(i); err != nil {
			return nil, fmt.Errorf("derive %v: %w", path, err)
		}
	}
	return k, nil
}

func (s *Set) derivePriv(path ...uint32) (*btcec.PrivateKey, error) {
	k, err := s.deriveExt(path...)
	if err != nil {
		return nil, err
	}
	return k.ECPrivKey()
}

// NostrSecretHex is the identity secret key as go-nostr expects it.
func (s *Set) NostrSecretHex() string { return hex.EncodeToString(s.Nostr.Serialize()) }

// NostrPubHex is the x-only identity public key.
func (s *Set) NostrPubHex() string {
	return hex.EncodeToString(schnorr.SerializePubKey(s.Nostr.PubKey()))
}

// WalletAddress is the P2WPKH address of m/84'/1'/0'/0/0.
func (s *Set) WalletAddress() string { return P2WPKHAddress(s.Wallet.PubKey()) }

// EVMAddress is the account of m/44'/60'/0'/0/0.
func (s *Set) EVMAddress() common.Address {
	return crypto.PubkeyToAddress(*s.EVM.PubKey().ToECDSA())
}

// OrderKey is the user/shopper key of an order: m/7333'/1'/{idx}'.
func (s *Set) OrderKey(orderID string) (*btcec.PrivateKey, error) {
	idx, err := OrderIndex(orderID)
	if err != nil {
		return nil, err
	}
	return s.derivePriv(append(PathOrder[:2:2], idx+hardened)...)
}

// EscrowXpub is the extended public key of m/7333'/2' (tpub…) published in the escrow profile.
func (s *Set) EscrowXpub() (string, error) {
	k, err := s.deriveExt(PathEscrow...)
	if err != nil {
		return "", err
	}
	pub, err := k.Neuter()
	if err != nil {
		return "", err
	}
	return pub.String(), nil
}

// EscrowOrderKey is the escrow private key of an order: m/7333'/2'/{idx}.
func (s *Set) EscrowOrderKey(orderID string) (*btcec.PrivateKey, error) {
	idx, err := OrderIndex(orderID)
	if err != nil {
		return nil, err
	}
	return s.derivePriv(append(PathEscrow[:2:2], idx)...)
}

// EscrowChildPubKey derives the escrow order key {idx} from a published tpub.
func EscrowChildPubKey(xpub, orderID string) (*btcec.PublicKey, error) {
	idx, err := OrderIndex(orderID)
	if err != nil {
		return nil, err
	}
	k, err := hdkeychain.NewKeyFromString(xpub)
	if err != nil {
		return nil, fmt.Errorf("parse xpub: %w", err)
	}
	if k.IsPrivate() {
		return nil, errors.New("expected an extended public key")
	}
	child, err := k.Derive(idx)
	if err != nil {
		return nil, fmt.Errorf("derive escrow child: %w", err)
	}
	return child.ECPubKey()
}

// OrderIndex implements spec §1.1.
func OrderIndex(orderID string) (uint32, error) {
	raw, err := OrderIDBytes(orderID)
	if err != nil {
		return 0, err
	}
	h := sha256.Sum256(raw)
	return binary.BigEndian.Uint32(h[:4]) & 0x7fffffff, nil
}

// OrderIDBytes decodes and checks an order id.
func OrderIDBytes(orderID string) ([]byte, error) {
	raw, err := hex.DecodeString(orderID)
	if err != nil || len(raw) != 16 || strings.ToLower(orderID) != orderID {
		return nil, errBadOrder
	}
	return raw, nil
}

// P2WPKHAddress encodes a compressed public key as a tb1q… address.
func P2WPKHAddress(pub *btcec.PublicKey) string {
	addr, err := btcutil.NewAddressWitnessPubKeyHash(btcutil.Hash160(pub.SerializeCompressed()), BTCParams)
	if err != nil {
		panic(err) // a 20 byte hash always encodes
	}
	return addr.EncodeAddress()
}

// ParsePubKeyHex parses a 33 byte compressed public key.
func ParsePubKeyHex(s string) (*btcec.PublicKey, error) {
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 33 {
		return nil, fmt.Errorf("expected a 33 byte compressed public key")
	}
	return btcec.ParsePubKey(raw)
}
