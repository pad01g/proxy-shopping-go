package proto

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestWireShapes(t *testing.T) {
	zero := uint32(0)
	data, _ := json.Marshal(OrderFunded{Asset: AssetBTC, TxID: "aa", Vout: &zero, Amount: "1"})
	if !strings.Contains(string(data), `"vout":0`) {
		t.Fatalf("vout 0 must be sent: %s", data)
	}
	data, _ = json.Marshal(OrderFunded{Asset: AssetUSDC, Safe: "0x1", Amount: "1"})
	if strings.Contains(string(data), "vout") || strings.Contains(string(data), "txid") {
		t.Fatalf("BTC fields in a USDC funding: %s", data)
	}
	data, _ = json.Marshal(OrderQuote{Accept: false, RejectReason: RejectRisk, Detail: "d"})
	if string(data) != `{"accept":false,"reject_reason":"risk","detail":"d"}` {
		t.Fatalf("rejection: %s", data)
	}
	var ts TrackingStatus
	if err := json.Unmarshal([]byte(`{"status":"delivered","updated_at":1790000000,"evidence":[]}`), &ts); err != nil || string(ts.UpdatedAt) != "1790000000" {
		t.Fatalf("%+v %v", ts, err)
	}
}

func TestRequestCommitsToTheEscrowKey(t *testing.T) {
	data, _ := json.Marshal(OrderRequest{Delivery: Delivery{Ciphertext: "c", KeyForShopper: "k", KeyForEscrowSHA256: EscrowKeyHash("x")}, KeyProof: "p"})
	if strings.Contains(string(data), `"key_for_escrow"`) || !strings.Contains(string(data), `"key_for_escrow_sha256":"2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"`) || !strings.Contains(string(data), `"key_proof":"p"`) {
		t.Fatalf("request: %s", data)
	}
}
