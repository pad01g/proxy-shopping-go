// esplora-lite serves an Esplora-compatible HTTP API from bitcoind (txindex=1) for the lab.
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
	"github.com/pad01g/proxy-shopping-go/node/internal/esplora"
	"github.com/pad01g/proxy-shopping-go/node/internal/keys"
)

func main() {
	listen := flag.String("listen", "0.0.0.0:3000", "HTTP listen address")
	rpcURL := flag.String("rpc", "http://lab:lab@127.0.0.1:38332", "bitcoind RPC URL with credentials")
	poll := flag.Duration("poll", time.Second, "how often to poll bitcoind for blocks and the mempool")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil)).With("service", "esplora-lite")

	rpc, err := bitcoinrpc.New(*rpcURL)
	if err != nil {
		log.Error("bad rpc url", "err", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ix := esplora.NewIndex(rpc, keys.BTCParams, log)
	// the first sync scans from genesis; retry while bitcoind starts
	for {
		err := ix.Sync(ctx)
		if err == nil {
			break
		}
		log.Info("waiting for bitcoind", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
	h, _ := ix.Tip()
	log.Info("indexed", "height", h)
	go ix.Run(ctx, *poll)

	srv := &http.Server{Addr: *listen, Handler: esplora.NewServer(ix, log), ReadHeaderTimeout: 10 * time.Second}
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
