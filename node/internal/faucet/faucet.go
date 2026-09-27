// Package faucet is the lab faucet (docs/lab.md "蛇口"): signet coins from a bitcoind wallet, ETH through
// anvil_setBalance, MockUSDC through mint, block mining and EVM time travel.
package faucet

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/ethereum/go-ethereum/common"

	"github.com/pad01g/proxy-shopping-go/node/internal/bitcoinrpc"
	"github.com/pad01g/proxy-shopping-go/node/internal/evm"
	"github.com/pad01g/proxy-shopping-go/node/internal/keys"
)

// AnvilKey0 is the private key of anvil's default account 0.
const AnvilKey0 = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"

// Config configures a faucet.
type Config struct {
	BTC             *bitcoinrpc.Client // node endpoint
	Wallet          string
	EVMURL          string // empty disables the EVM endpoints
	DeploymentsPath string
	EVMKey          *btcec.PrivateKey
	Log             *slog.Logger
}

// Faucet serves the faucet API.
type Faucet struct {
	cfg    Config
	node   *bitcoinrpc.Client
	wallet *bitcoinrpc.Client
	log    *slog.Logger

	mineMu   sync.Mutex
	mineAddr string

	evmMu sync.Mutex // guards the fields below and serializes EVM sends (nonces)
	ec    *evm.Client
	dep   *evm.Deployments
}

// New returns a faucet; call Init before serving.
func New(cfg Config) *Faucet {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Wallet == "" {
		cfg.Wallet = "faucet"
	}
	return &Faucet{cfg: cfg, node: cfg.BTC, wallet: cfg.BTC.Wallet(cfg.Wallet), log: cfg.Log}
}

// ParseKey parses a hex private key (0x optional).
func ParseKey(s string) (*btcec.PrivateKey, error) {
	raw, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(s), "0x"))
	if err != nil || len(raw) != 32 {
		return nil, errors.New("expected a 32 byte hex private key")
	}
	k, _ := btcec.PrivKeyFromBytes(raw)
	return k, nil
}

// Init waits for bitcoind, loads or creates the wallet and mines spendable coins when the wallet is poor.
func (f *Faucet) Init(ctx context.Context, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		_, err := f.node.GetBlockCount(ctx)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("bitcoind not reachable: %w", err)
		}
		f.log.Info("waiting for bitcoind", "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	if err := f.node.EnsureWallet(ctx, f.cfg.Wallet); err != nil {
		return err
	}
	addr, err := f.wallet.GetNewAddress(ctx)
	if err != nil {
		return fmt.Errorf("mining address: %w", err)
	}
	f.mineAddr = addr
	bal, err := f.wallet.GetBalance(ctx)
	if err != nil {
		return fmt.Errorf("wallet balance: %w", err)
	}
	if bal < 10 {
		// coinbase outputs mature after 100 blocks
		f.log.Info("funding the faucet wallet", "balance", bal)
		if _, err := f.Mine(ctx, 110); err != nil {
			return err
		}
	}
	return nil
}

// Mine mines n blocks to the faucet wallet and returns the new height.
func (f *Faucet) Mine(ctx context.Context, n int) (int64, error) {
	f.mineMu.Lock()
	defer f.mineMu.Unlock()
	if _, err := f.node.GenerateToAddress(ctx, n, f.mineAddr); err != nil {
		return 0, fmt.Errorf("mine %d blocks: %w", n, err)
	}
	return f.node.GetBlockCount(ctx)
}

// AutoMine mines one block whenever the mempool is not empty.
func (f *Faucet) AutoMine(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		ids, err := f.node.GetRawMempool(ctx)
		if err != nil {
			f.log.Warn("auto-mine: mempool", "err", err)
			continue
		}
		if len(ids) > 0 {
			if _, err := f.Mine(ctx, 1); err != nil {
				f.log.Warn("auto-mine", "err", err)
			}
		}
	}
}

// SatsToBTC formats sats as a BTC amount with 8 decimals.
func SatsToBTC(sats int64) string { return fmt.Sprintf("%d.%08d", sats/1e8, sats%1e8) }

// SendBTC pays sats to an address and mines a block.
func (f *Faucet) SendBTC(ctx context.Context, address string, sats int64) (string, error) {
	a, err := btcutil.DecodeAddress(address, keys.BTCParams)
	if err != nil || !a.IsForNet(keys.BTCParams) {
		return "", badRequest("not a signet address: " + address)
	}
	if sats <= 0 {
		return "", badRequest("sats must be positive")
	}
	txid, err := f.wallet.SendToAddress(ctx, a.EncodeAddress(), SatsToBTC(sats))
	if err != nil {
		return "", fmt.Errorf("send: %w", err)
	}
	if _, err := f.Mine(ctx, 1); err != nil {
		return "", err
	}
	return txid, nil
}

func (f *Faucet) evmClient(ctx context.Context) (*evm.Client, error) {
	if f.cfg.EVMURL == "" {
		return nil, errors.New("EVM is not configured")
	}
	if f.ec == nil {
		ec, err := evm.Dial(ctx, f.cfg.EVMURL, nil)
		if err != nil {
			return nil, err
		}
		f.ec = ec
	}
	return f.ec, nil
}

// deployments loads the deployments file, waiting a little for the deployer to write it.
func (f *Faucet) deployments(ctx context.Context) (*evm.Deployments, error) {
	if f.dep != nil {
		return f.dep, nil
	}
	var err error
	for i := 0; i < 10; i++ {
		var d *evm.Deployments
		if d, err = evm.LoadDeployments(f.cfg.DeploymentsPath); err == nil {
			f.dep = d
			return d, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return nil, fmt.Errorf("deployments not available: %w", err)
}

// decimalUnits converts a decimal string to integer units with the given decimals.
func decimalUnits(s string, decimals int) (*big.Int, error) {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(s))
	if !ok || r.Sign() < 0 {
		return nil, fmt.Errorf("%q is not a non-negative decimal", s)
	}
	r.Mul(r, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)))
	if !r.IsInt() {
		return nil, fmt.Errorf("%q has more than %d decimals", s, decimals)
	}
	return r.Num(), nil
}

// FundEVM sets the ETH balance and mints USDC; empty amounts are skipped. It returns the mint tx hash.
func (f *Faucet) FundEVM(ctx context.Context, address, eth, usdc string) (string, error) {
	if !common.IsHexAddress(address) {
		return "", badRequest("not an EVM address: " + address)
	}
	to := common.HexToAddress(address)
	var wei, units *big.Int
	var err error
	if eth != "" {
		if wei, err = decimalUnits(eth, 18); err != nil {
			return "", badRequest("eth: " + err.Error())
		}
	}
	if usdc != "" {
		if units, err = decimalUnits(usdc, 6); err != nil {
			return "", badRequest("usdc: " + err.Error())
		}
	}
	f.evmMu.Lock()
	defer f.evmMu.Unlock()
	ec, err := f.evmClient(ctx)
	if err != nil {
		return "", err
	}
	if wei != nil {
		if err := ec.RPC.CallContext(ctx, nil, "anvil_setBalance", to, "0x"+wei.Text(16)); err != nil {
			return "", fmt.Errorf("anvil_setBalance: %w", err)
		}
	}
	if units == nil || units.Sign() == 0 {
		return "", nil
	}
	dep, err := f.deployments(ctx)
	if err != nil {
		return "", err
	}
	data, err := evm.ERC20ABI.Pack("mint", to, units)
	if err != nil {
		return "", err
	}
	r, err := ec.SendAndWait(ctx, f.cfg.EVMKey, dep.USDC, data)
	if err != nil {
		return "", fmt.Errorf("mint: %w", err)
	}
	return r.TxHash.Hex(), nil
}

// AdvanceEVMTime moves anvil's clock forward and mines a block; it returns the new block time.
func (f *Faucet) AdvanceEVMTime(ctx context.Context, seconds int64) (uint64, error) {
	if seconds < 0 {
		return 0, badRequest("seconds must not be negative")
	}
	f.evmMu.Lock()
	defer f.evmMu.Unlock()
	ec, err := f.evmClient(ctx)
	if err != nil {
		return 0, err
	}
	if err := ec.RPC.CallContext(ctx, nil, "evm_increaseTime", seconds); err != nil {
		return 0, fmt.Errorf("evm_increaseTime: %w", err)
	}
	if err := ec.RPC.CallContext(ctx, nil, "evm_mine"); err != nil {
		return 0, fmt.Errorf("evm_mine: %w", err)
	}
	return ec.LatestTime(ctx)
}

// Height returns the BTC height and, if the EVM answers, its latest block time.
func (f *Faucet) Height(ctx context.Context) (int64, *uint64, error) {
	h, err := f.node.GetBlockCount(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("block count: %w", err)
	}
	f.evmMu.Lock()
	defer f.evmMu.Unlock()
	ec, err := f.evmClient(ctx)
	if err != nil {
		return h, nil, nil
	}
	tctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	t, err := ec.LatestTime(tctx)
	if err != nil {
		f.log.Debug("evm time unavailable", "err", err)
		return h, nil, nil
	}
	return h, &t, nil
}

type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return e.msg }

func badRequest(msg string) error { return &httpError{code: http.StatusBadRequest, msg: msg} }

// flexInt accepts 5 or "5".
type flexInt int64

func (n *flexInt) UnmarshalJSON(b []byte) error {
	v, err := strconv.ParseInt(strings.Trim(string(b), `"`), 10, 64)
	if err != nil {
		return fmt.Errorf("expected an integer, got %s", b)
	}
	*n = flexInt(v)
	return nil
}

// flexDecimal accepts "1.5" or 1.5.
type flexDecimal string

func (d *flexDecimal) UnmarshalJSON(b []byte) error {
	*d = flexDecimal(strings.Trim(string(b), `"`))
	return nil
}

// Handler returns the HTTP API with CORS.
func (f *Faucet) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /btc", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Address string  `json:"address"`
			Sats    flexInt `json:"sats"`
		}
		if !decode(w, r, &req) {
			return
		}
		txid, err := f.SendBTC(r.Context(), req.Address, int64(req.Sats))
		reply(w, map[string]string{"txid": txid}, err)
	})
	mux.HandleFunc("POST /evm", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Address string      `json:"address"`
			ETH     flexDecimal `json:"eth"`
			USDC    flexDecimal `json:"usdc"`
		}
		if !decode(w, r, &req) {
			return
		}
		tx, err := f.FundEVM(r.Context(), req.Address, string(req.ETH), string(req.USDC))
		reply(w, map[string]string{"tx": tx}, err)
	})
	mux.HandleFunc("POST /mine", func(w http.ResponseWriter, r *http.Request) {
		req := struct {
			Blocks flexInt `json:"blocks"`
		}{Blocks: 1}
		if !decode(w, r, &req) {
			return
		}
		if req.Blocks <= 0 || req.Blocks > 10000 {
			reply(w, nil, badRequest("blocks must be between 1 and 10000"))
			return
		}
		h, err := f.Mine(r.Context(), int(req.Blocks))
		reply(w, map[string]int64{"height": h}, err)
	})
	mux.HandleFunc("POST /evm/time", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Seconds flexInt `json:"seconds"`
		}
		if !decode(w, r, &req) {
			return
		}
		t, err := f.AdvanceEVMTime(r.Context(), int64(req.Seconds))
		reply(w, map[string]uint64{"evm_time": t}, err)
	})
	mux.HandleFunc("GET /height", func(w http.ResponseWriter, r *http.Request) {
		h, t, err := f.Height(r.Context())
		reply(w, map[string]any{"btc": h, "evm_time": t}, err)
	})
	return cors(mux, f.log)
}

func cors(next http.Handler, _ *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	// an empty body is fine: POST /mine without body mines one block
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		reply(w, nil, badRequest("bad JSON body: "+err.Error()))
		return false
	}
	return true
}

func reply(w http.ResponseWriter, v any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		code := http.StatusBadGateway
		var he *httpError
		if errors.As(err, &he) {
			code = he.code
		}
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}
