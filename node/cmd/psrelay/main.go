// psrelay is the mailbox Nostr relay of proxy-shopping (khatru with a badger event store).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pad01g/proxy-shopping-go/node/internal/relay"
)

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return def
}

func parseKinds(s string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if lo, hi, ok := strings.Cut(part, "-"); ok {
			a, err1 := strconv.Atoi(lo)
			b, err2 := strconv.Atoi(hi)
			if err1 != nil || err2 != nil || a > b {
				return nil, fmt.Errorf("bad kind range %q", part)
			}
			for k := a; k <= b; k++ {
				out = append(out, k)
			}
			continue
		}
		k, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("bad kind %q", part)
		}
		out = append(out, k)
	}
	return out, nil
}

func main() {
	listen := flag.String("listen", env("PSRELAY_LISTEN", "0.0.0.0:7777"), "listen address")
	data := flag.String("data", env("PSRELAY_DATA", "data"), "event store directory")
	kinds := flag.String("kinds", env("PSRELAY_KINDS", "0,5,1059,10050,30500-30503"), "accepted kinds (list and ranges)")
	retention := flag.Int("retention-days", envInt("PSRELAY_RETENTION_DAYS", 30), "days to keep gift wraps (kind 1059)")
	maxSize := flag.Int("max-event-size", envInt("PSRELAY_MAX_EVENT_SIZE", 256<<10), "maximum serialized event size in bytes")
	name := flag.String("name", env("PSRELAY_NAME", "psrelay"), "NIP-11 name")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil)).With("service", "psrelay")

	ks, err := parseKinds(*kinds)
	if err != nil {
		log.Error("kinds", "err", err)
		os.Exit(2)
	}
	srv, err := relay.New(relay.Options{DataDir: *data, Kinds: ks, RetentionDays: *retention, MaxEventSize: *maxSize, Name: *name, Log: log})
	if err != nil {
		log.Error("start", "err", err)
		os.Exit(1)
	}
	defer srv.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go srv.RunPruner(ctx, time.Hour)

	hs := &http.Server{Addr: *listen, Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	log.Info("listening", "addr", *listen, "kinds", ks, "retention_days", *retention)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}
