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
