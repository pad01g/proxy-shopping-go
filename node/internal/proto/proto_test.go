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

func TestReplyP2PValidate(t *testing.T) {
	const id = "16Uiu2HAmE2FwVbZbrdbXNM6UsSRZwM4tQJ696kUmfpCNXYuz5HSa"
	const relay = "16Uiu2HAmFngaVFK4D5fix5qN2c6guh3LNyBkmuB42vTFW9aaeDZh"
	ok := []P2PContact{
		{PeerID: id},
		{PeerID: id, Addrs: []string{"/dns4/r.example/tcp/443/tls/ws/p2p/" + relay + "/p2p-circuit/p2p/" + id, "/ip4/1.2.3.4/tcp/4001"}},
	}
	for _, c := range ok {
		if err := c.Validate(); err != nil {
			t.Errorf("%+v: %v", c, err)
		}
	}
	many := P2PContact{PeerID: id}
	for i := 0; i <= MaxReplyP2PAddrs; i++ {
		many.Addrs = append(many.Addrs, "/ip4/1.2.3.4/tcp/4001")
	}
	bad := []P2PContact{
		{PeerID: "nope"},
		{PeerID: id, Addrs: []string{"not a multiaddr"}},
		{PeerID: id, Addrs: []string{"/ip4/1.2.3.4/tcp/4001/p2p/" + relay}},
		many,
	}
	for _, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("%+v accepted", c)
		}
	}
}
