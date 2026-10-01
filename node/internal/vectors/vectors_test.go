package vectors

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	p2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/nbd-wtf/go-nostr"

	"github.com/pad01g/proxy-shopping-go/node/internal/delivery"
	"github.com/pad01g/proxy-shopping-go/node/internal/giftwrap"
	"github.com/pad01g/proxy-shopping-go/node/internal/keys"
	"github.com/pad01g/proxy-shopping-go/node/internal/p2p"
	"github.com/pad01g/proxy-shopping-go/node/internal/trust"
)

var (
	keyDir     = filepath.Join("..", "..", "..", "lab", "keys")
	vectorFile = filepath.Join("..", "..", "..", "docs", "test-vectors.json")
)

func generate(t *testing.T) []byte {
	t.Helper()
	f, err := Generate(keyDir)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(f); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestVectorsFile keeps docs/test-vectors.json in sync with the implementation.
func TestVectorsFile(t *testing.T) {
	got := generate(t)
	if os.Getenv("UPDATE_VECTORS") != "" {
		if err := os.WriteFile(vectorFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(vectorFile)
	if err != nil {
		t.Fatalf("%v (run with UPDATE_VECTORS=1)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("docs/test-vectors.json is stale; run UPDATE_VECTORS=1 go test ./internal/vectors")
	}
}

func TestVectorsAreSelfConsistent(t *testing.T) {
	f, err := Generate(keyDir)
	if err != nil {
		t.Fatal(err)
	}
	shopper, err := keys.LoadMnemonicFile(filepath.Join(keyDir, "shopper-1.mnemonic"))
	if err != nil {
		t.Fatal(err)
	}
	user, _ := keys.LoadMnemonicFile(filepath.Join(keyDir, "user-1.mnemonic"))

	// gift wrap opens to the inner event
	wrap := f.GiftWrap["wrap"].(*nostr.Event)
	inner, err := giftwrap.Unwrap(shopper.NostrSecretHex(), wrap)
	if err != nil {
		t.Fatal(err)
	}
	if inner.ID != f.GiftWrap["inner"].(*nostr.Event).ID {
		t.Fatal("unwrapped a different inner event")
	}

	// delivery ciphertext and keys
	var k [32]byte
	raw, _ := hex.DecodeString(f.Delivery["k"].(string))
	copy(k[:], raw)
	addr, err := delivery.Open(k, f.Delivery["ciphertext"].(string), OrderID)
	if err != nil {
		t.Fatal(err)
	}
	js, _ := json.Marshal(addr)
	if string(js) != f.Delivery["address_json"] {
		t.Fatalf("address json %s", js)
	}
	kfs := f.Delivery["key_for_shopper"].(map[string]string)["payload"]
	k2, err := delivery.UnwrapKey(shopper.NostrSecretHex(), user.NostrPubHex(), kfs)
	if err != nil || k2 != k {
		t.Fatalf("key_for_shopper: %v", err)
	}

	// key proofs verify with the published keys
	kp := f.KeyProof
	b, ev := kp["btc"].(map[string]string), kp["evm"].(map[string]string)
	if err := keys.VerifyKeyProofBTC(b["user_btc_pubkey"], OrderID, user.NostrPubHex(), b["key_proof"]); err != nil {
		t.Fatalf("btc key_proof: %v", err)
	}
	if err := keys.VerifyKeyProofEVM(ev["user_evm_address"], OrderID, user.NostrPubHex(), ev["key_proof"]); err != nil {
		t.Fatalf("evm key_proof: %v", err)
	}

	// the CREATE2 address follows the formula of §6.2
	init, _ := hex.DecodeString(f.Safe["initializer"].(string)[2:])
	code, _ := hex.DecodeString(LabProxyCreationCode)
	saltHex, _ := hex.DecodeString(f.Safe["salt_nonce_hex"].(string)[2:])
	salt := crypto.Keccak256(crypto.Keccak256(init), saltHex)
	initCode := append(code, common.LeftPadBytes(common.HexToAddress(f.Safe["singleton"].(string)).Bytes(), 32)...)
	want := crypto.CreateAddress2(common.HexToAddress(f.Safe["factory"].(string)), [32]byte(salt), crypto.Keccak256(initCode))
	if want.Hex() != f.Safe["address"] {
		t.Fatalf("safe address %s, want %s", f.Safe["address"], want.Hex())
	}
}

func TestLabProxyCreationCodeMatchesContracts(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "abi", "SafeProxy.creationCode.hex"))
	if err != nil {
		t.Skipf("contracts not built: %v", err)
	}
	if got := strings.TrimPrefix(strings.TrimSpace(string(data)), "0x"); got != LabProxyCreationCode {
		t.Fatal("LabProxyCreationCode differs from contracts/abi/SafeProxy.creationCode.hex")
	}
}

// The p2p vectors: the bundle verifies event by event and in bundle order, the peer id is that of the protobuf
// public key, and the /ps/msg request line is the gift wrap for shopper-1.
func TestP2PVectors(t *testing.T) {
	f, err := Generate(keyDir)
	if err != nil {
		t.Fatal(err)
	}
	b := f.P2P["bundle"].(*trust.Bundle)
	kinds := []int{}
	for _, ev := range b.Events {
		if err := trust.Validate(ev); err != nil {
			t.Fatalf("bundle event kind %d: %v", ev.Kind, err)
		}
		kinds = append(kinds, ev.Kind)
	}
	if fmt.Sprint(kinds) != "[30500 30501 30502 10050]" {
		t.Fatalf("bundle order %v", kinds)
	}
	if urls := trust.ListURLs(b.Events[0]); len(urls) != 1 || urls[0] != f.P2P["list_url"] {
		t.Fatalf("list_url %v", urls)
	}
	raw, _ := hex.DecodeString(f.Libp2p["pubkey_protobuf"].(string))
	pub, err := p2pcrypto.UnmarshalPublicKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := peer.IDFromPublicKey(pub)
	if id.String() != f.Libp2p["peer_id"] || id.String() != f.Keys["abandon"].Libp2pPeerID {
		t.Fatalf("peer id %s", id)
	}
	shopper, _ := keys.LoadMnemonicFile(filepath.Join(keyDir, "shopper-1.mnemonic"))
	line := strings.TrimSuffix(f.P2P["msg"].(map[string]any)["request_line"].(string), "\n")
	if _, err := p2p.CheckMsgLine([]byte(line), shopper.NostrPubHex()); err != nil {
		t.Fatalf("request line: %v", err)
	}
}
