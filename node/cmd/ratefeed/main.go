// Command ratefeed copies the rates of the fx sources (the lab ratemock) into the MockAggregatorV3 feeds, so that
// the chainlink provider sees the same values as frankfurter and coingecko (spec §7).
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/ethereum/go-ethereum/common"
	"gopkg.in/yaml.v3"

	"github.com/pad01g/proxy-shopping-go/node/internal/config"
	"github.com/pad01g/proxy-shopping-go/node/internal/evm"
	"github.com/pad01g/proxy-shopping-go/node/internal/fx"
	"github.com/pad01g/proxy-shopping-go/node/internal/httpx"
)

// defaultKey is the private key of anvil's second default account (0x70997970…79C8). The first one belongs to
// the deployer and labfaucet; sharing it made their transactions race for the same nonces.
const defaultKey = "59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d"

// feedPairs are the pairs of the lab feeds.
var feedPairs = []string{"BTC/USD", "JPY/USD", "USDC/USD"}

// Config is the ratefeed YAML.
type Config struct {
	IntervalSeconds int `yaml:"interval_seconds"`
	EVM             struct {
		RPC         string `yaml:"rpc"`
		Deployments string `yaml:"deployments"`
		Key         string `yaml:"key"` // hex private key of the sender (RATEFEED_KEY overrides; default anvil account 1)
	} `yaml:"evm"`
	TLS config.TLS `yaml:"tls"`
	FX  config.FX  `yaml:"fx"`
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("ratefeed failed", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfgPath := flag.String("config", "", "YAML configuration")
	rpc := flag.String("rpc", "", "EVM RPC URL (overrides evm.rpc)")
	deployments := flag.String("deployments", "", "deployments JSON (overrides evm.deployments)")
	interval := flag.Int("interval", 0, "seconds between updates (overrides interval_seconds)")
	extraCA := flag.String("extra-ca", "", "extra CA PEM (overrides tls.extra_ca)")
	flag.Parse()

	var cfg Config
	if *cfgPath != "" {
		data, err := os.ReadFile(*cfgPath)
		if err != nil {
			return fmt.Errorf("read config: %w", err)
		}
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return fmt.Errorf("parse config: %w", err)
		}
	}
	override(&cfg.EVM.RPC, *rpc)
	override(&cfg.EVM.Deployments, *deployments)
	override(&cfg.TLS.ExtraCA, *extraCA)
	if *interval > 0 {
		cfg.IntervalSeconds = *interval
	}
	if cfg.IntervalSeconds <= 0 {
		cfg.IntervalSeconds = 5
	}
	if cfg.EVM.RPC == "" || cfg.EVM.Deployments == "" {
		return errors.New("evm.rpc and evm.deployments are required")
	}
	override(&cfg.EVM.Key, os.Getenv("RATEFEED_KEY"))
	key, err := loadKey(cfg.EVM.Key)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.TLS.ExtraCA != "" {
		if err := httpx.WaitForFile(cfg.TLS.ExtraCA, 2*time.Minute); err != nil {
			return fmt.Errorf("extra CA: %w", err)
		}
	}
	tc, err := httpx.TLSConfig(cfg.TLS.ExtraCA)
	if err != nil {
		return err
	}
	hc := httpx.Client(tc, 15*time.Second)

	var sources []config.FXSource
	for _, s := range cfg.FX.Sources {
		if s.Type != "chainlink" { // never feed the oracle from itself
			sources = append(sources, s)
		}
	}
	providers, err := fx.FromConfig(sources, []string{"JPY"}, hc, nil, nil)
	if err != nil {
		return err
	}
	if len(providers) == 0 {
		return errors.New("no fx sources configured")
	}
	rates := fx.New(providers, log)

	d, err := waitDeployments(ctx, cfg.EVM.Deployments, log)
	if err != nil {
		return err
	}
	client, err := waitRPC(ctx, cfg.EVM.RPC, hc, log)
	if err != nil {
		return err
	}
	f := &feeder{rates: rates, client: client, key: key, feeds: d.Feeds, last: map[string]*big.Int{}, log: log}
	log.Info("ratefeed started", "rpc", cfg.EVM.RPC, "interval", cfg.IntervalSeconds, "feeds", len(d.Feeds))
	t := time.NewTicker(time.Duration(cfg.IntervalSeconds) * time.Second)
	defer t.Stop()
	for {
		f.update(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func override(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}

func loadKey(s string) (*btcec.PrivateKey, error) {
	if s == "" {
		s = defaultKey
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(s), "0x"))
	if err != nil || len(raw) != 32 {
		return nil, errors.New("evm.key / RATEFEED_KEY must be 32 bytes of hex")
	}
	k, _ := btcec.PrivKeyFromBytes(raw)
	return k, nil
}

func waitDeployments(ctx context.Context, path string, log *slog.Logger) (*evm.Deployments, error) {
	for {
		d, err := evm.LoadDeployments(path)
		if err == nil {
			return d, nil
		}
		log.Info("waiting for deployments", "path", path, "err", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func waitRPC(ctx context.Context, url string, hc *http.Client, log *slog.Logger) (*evm.Client, error) {
	for {
		c, err := evm.Dial(ctx, url, hc)
		if err == nil {
			if _, err = c.Eth.ChainID(ctx); err == nil {
				return c, nil
			}
		}
		log.Info("waiting for EVM RPC", "rpc", url, "err", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// ratesSource is what the feeder needs of fx.Service.
type ratesSource interface {
	Quote(ctx context.Context, pair string) (*fx.Quote, error)
}

type feeder struct {
	rates  ratesSource
	client *evm.Client
	key    *btcec.PrivateKey
	feeds  map[string]common.Address
	last   map[string]*big.Int
	log    *slog.Logger
	// write sends setAnswer; tests replace it
	write func(ctx context.Context, feed common.Address, answer *big.Int) error
}

// update computes every feed value and writes the ones that changed.
func (f *feeder) update(ctx context.Context) {
	for _, pair := range feedPairs {
		feed, ok := f.feeds[pair]
		if !ok {
			continue
		}
		q, err := f.rates.Quote(ctx, pair)
		if err != nil {
			f.log.Warn("no rate", "pair", pair, "err", err)
			continue
		}
		answer := Scale8(q.Rate)
		if !f.changed(pair, answer) {
			continue
		}
		if err := f.send(ctx, feed, answer); err != nil {
			f.log.Warn("setAnswer failed", "pair", pair, "err", err)
			continue
		}
		f.last[pair] = answer
		f.log.Info("feed updated", "pair", pair, "rate", fx.FormatRate(q.Rate), "answer", answer)
	}
}

func (f *feeder) changed(pair string, answer *big.Int) bool {
	prev, ok := f.last[pair]
	return !ok || prev.Cmp(answer) != 0
}

func (f *feeder) send(ctx context.Context, feed common.Address, answer *big.Int) error {
	if f.write != nil {
		return f.write(ctx, feed, answer)
	}
	data, err := evm.AggregatorABI.Pack("setAnswer", answer)
	if err != nil {
		return err
	}
	_, err = f.client.SendAndWait(ctx, f.key, feed, data)
	return err
}

// Scale8 converts a rate to the 8 decimal integer of the feeds, rounding half up.
func Scale8(r *big.Rat) *big.Int {
	return fx.RoundHalfUp(new(big.Rat).Mul(r, big.NewRat(100_000_000, 1)))
}
