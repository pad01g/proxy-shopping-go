// labfaucet hands out lab coins (BTC signet, ETH and MockUSDC on anvil), mines blocks and moves EVM time.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pad01g/proxy-shopping-go/node/internal/bitcoinrpc"
	"github.com/pad01g/proxy-shopping-go/node/internal/faucet"
)

func main() {
	listen := flag.String("listen", "0.0.0.0:8080", "HTTP listen address")
	btcRPC := flag.String("btc-rpc", "http://lab:lab@127.0.0.1:38332", "bitcoind RPC URL with credentials")
	evmRPC := flag.String("evm-rpc", "http://127.0.0.1:8545", "anvil RPC URL (empty disables EVM)")
	deployments := flag.String("deployments", "/deployments/31337.json", "contracts deployments file")
	wallet := flag.String("wallet", "faucet", "bitcoind wallet name")
	autoMine := flag.Duration("auto-mine", time.Second, "mine a block when the mempool is not empty, this often (0 disables)")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil)).With("service", "labfaucet")

	rpc, err := bitcoinrpc.New(*btcRPC)
	if err != nil {
		log.Error("bad btc rpc url", "err", err)
		os.Exit(2)
	}
	keyHex := os.Getenv("FAUCET_EVM_KEY")
	if keyHex == "" {
		keyHex = faucet.AnvilKey0
	}
	key, err := faucet.ParseKey(keyHex)
	if err != nil {
		log.Error("FAUCET_EVM_KEY", "err", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	f := faucet.New(faucet.Config{BTC: rpc, Wallet: *wallet, EVMURL: *evmRPC, DeploymentsPath: *deployments, EVMKey: key, Log: log})
	if err := f.Init(ctx, 5*time.Minute); err != nil {
		log.Error("init", "err", err)
		os.Exit(1)
	}
	if *autoMine > 0 {
		go f.AutoMine(ctx, *autoMine)
	}
	srv := &http.Server{Addr: *listen, Handler: f.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("listening", "addr", *listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}
