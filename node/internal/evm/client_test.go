package evm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
)

// fakeRPC answers the JSON-RPC calls of Send and Wait. Its pending nonce is the number of transactions it got,
// and it is slow to answer eth_getTransactionCount, so that unserialized senders would read the same nonce.
type fakeRPC struct {
	mu     sync.Mutex
	nonces []uint64
}

func (f *fakeRPC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		ID     json.RawMessage   `json:"id"`
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	_ = json.Unmarshal(body, &req)
	var result any
	switch req.Method {
	case "eth_chainId":
		result = "0x7a69"
	case "eth_getTransactionCount":
		time.Sleep(20 * time.Millisecond)
		f.mu.Lock()
		result = hexutil.Uint64(len(f.nonces))
		f.mu.Unlock()
	case "eth_estimateGas":
		result = "0x5208"
	case "eth_maxPriorityFeePerGas":
		result = "0x1"
	case "eth_getBlockByNumber":
		result = map[string]any{
			"parentHash": "0x" + fmt.Sprintf("%064x", 0), "sha3Uncles": "0x" + fmt.Sprintf("%064x", 0), "miner": "0x" + fmt.Sprintf("%040x", 0),
			"stateRoot": "0x" + fmt.Sprintf("%064x", 0), "transactionsRoot": "0x" + fmt.Sprintf("%064x", 0), "receiptsRoot": "0x" + fmt.Sprintf("%064x", 0),
			"logsBloom": "0x" + fmt.Sprintf("%0512x", 0), "difficulty": "0x0", "number": "0x1", "gasLimit": "0x1c9c380", "gasUsed": "0x0",
			"timestamp": "0x1", "extraData": "0x", "mixHash": "0x" + fmt.Sprintf("%064x", 0), "nonce": "0x0000000000000000", "baseFeePerGas": "0x1",
			"hash": "0x" + fmt.Sprintf("%064x", 1),
		}
	case "eth_sendRawTransaction":
		var raw hexutil.Bytes
		_ = json.Unmarshal(req.Params[0], &raw)
		var tx types.Transaction
		if err := tx.UnmarshalBinary(raw); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		f.mu.Lock()
		f.nonces = append(f.nonces, tx.Nonce())
		f.mu.Unlock()
		result = tx.Hash()
	case "eth_getTransactionReceipt":
		result = nil // never mined
	default:
		http.Error(w, "unexpected "+req.Method, 400)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

func TestSendSerializesNoncesAndWaitGivesUp(t *testing.T) {
	f := &fakeRPC{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	ctx := context.Background()
	c, err := Dial(ctx, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := btcec.NewPrivateKey()
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Send(ctx, key, addr(1), nil, nil); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	seen := map[uint64]bool{}
	for _, n := range f.nonces {
		if seen[n] {
			t.Fatalf("nonce %d used twice: %v", n, f.nonces)
		}
		seen[n] = true
	}

	// a transaction that is never mined does not block forever
	WaitTimeout = 300 * time.Millisecond
	defer func() { WaitTimeout = 3 * time.Minute }()
	start := time.Now()
	if _, err := c.Wait(ctx, [32]byte{1}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("wait ignored its timeout")
	}
}
