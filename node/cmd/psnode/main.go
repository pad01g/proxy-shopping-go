// psnode is the Go node of proxy-shopping: shopper, escrow, operator or p2p relay (docs/lab.md).
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/pad01g/proxy-shopping-go/node/internal/config"
	"github.com/pad01g/proxy-shopping-go/node/internal/node"
)

func main() {
	cfgPath := flag.String("config", "psnode.yaml", "YAML configuration")
	role := flag.String("role", "", "override the role of the configuration")
	debug := flag.Bool("debug", os.Getenv("PS_DEBUG") != "", "debug logging")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("configuration", "err", err)
		os.Exit(2)
	}
	if *role != "" {
		cfg.Role = *role
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	n, err := node.New(ctx, cfg, log)
	if err != nil {
		log.Error("start", "err", err)
		os.Exit(1)
	}
	if err := n.Run(ctx); err != nil {
		log.Error("stopped", "err", err)
		os.Exit(1)
	}
}
