// Package config reads the psnode YAML configuration (docs/lab.md).
package config

import (
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Roles of a node.
const (
	RoleShopper  = "shopper"
	RoleEscrow   = "escrow"
	RoleOperator = "operator"
	RoleRelay    = "relay"
)

// Config is the whole file.
type Config struct {
	Role         string   `yaml:"role"`
	Name         string   `yaml:"name"`
	Network      string   `yaml:"network"`
	MnemonicFile string   `yaml:"mnemonic_file"`
	DataDir      string   `yaml:"data_dir"`
	Admin        Admin    `yaml:"admin"`
	TLS          TLS      `yaml:"tls"`
	Nostr        Nostr    `yaml:"nostr"`
	P2P          P2P      `yaml:"p2p"`
	Trust        Trust    `yaml:"trust"`
	Chain        Chain    `yaml:"chain"`
	FX           FX       `yaml:"fx"`
	Shopper      *Shopper `yaml:"shopper"`
	Escrow       *Escrow  `yaml:"escrow"`
}

type Admin struct {
	Listen string `yaml:"listen"`
	Token  string `yaml:"token"`
}

type TLS struct {
	ExtraCA string `yaml:"extra_ca"`
}

type Nostr struct {
	Relays []string `yaml:"relays"`
	K      int      `yaml:"k"`
}

type P2P struct {
	Listen       []string `yaml:"listen"`
	Bootstrap    []string `yaml:"bootstrap"`
	Relays       []string `yaml:"relays"`
	Reachability string   `yaml:"reachability"` // auto | public | private
	Announce     []string `yaml:"announce"`     // optional extra addresses to announce
}

type Trust struct {
	Coordinators []string `yaml:"coordinators"`
}

type Chain struct {
	BTC *BTCChain `yaml:"btc"`
	EVM *EVMChain `yaml:"evm"`
}

type BTCChain struct {
	Network string `yaml:"network"`
	Esplora string `yaml:"esplora"`
}

type EVMChain struct {
	ChainID     int64  `yaml:"chain_id"`
	RPC         string `yaml:"rpc"`
	Deployments string `yaml:"deployments"`
}

type FX struct {
	Sources []FXSource `yaml:"sources"`
}

// FXSource configures one rate provider (§7).
type FXSource struct {
	Type  string            `yaml:"type"` // frankfurter | coingecko | chainlink | static
	Base  string            `yaml:"base"`
	Feeds map[string]string `yaml:"feeds"` // chainlink: pair → aggregator address; empty: the deployments' feeds
	Rates map[string]string `yaml:"rates"` // static: pair → rate
}

// Money is an amount with its currency (decimal string).
type Money struct {
	Amount   string `yaml:"amount" json:"amount"`
	Currency string `yaml:"currency" json:"currency"`
}

type Fee struct {
	BPS int64 `yaml:"bps"`
	Min Money `yaml:"min"`
}

type Risk struct {
	Allowlist     []string `yaml:"allowlist"`
	KnownGateways []string `yaml:"known_gateways"`
	Threshold     int      `yaml:"threshold"`
}

type Timelock struct {
	BTCT1Blocks  int64 `yaml:"btc_t1_blocks"`
	BTCT2Blocks  int64 `yaml:"btc_t2_blocks"`
	EVMT1Seconds int64 `yaml:"evm_t1_seconds"`
	EVMT2Seconds int64 `yaml:"evm_t2_seconds"`
}

// Shopper is the policy of a shopper node.
type Shopper struct {
	BotURL              string   `yaml:"bot_url"`
	Payments            []string `yaml:"payments"`
	Currencies          []string `yaml:"currencies"`
	CashRegions         []string `yaml:"cash_regions"`
	Fee                 Fee      `yaml:"fee"`
	MaxOrder            Money    `yaml:"max_order"`
	DeliveryDays        int64    `yaml:"delivery_days"`
	Risk                Risk     `yaml:"risk"`
	Timelock            Timelock `yaml:"timelock"`
	Confirmations       int64    `yaml:"confirmations"`
	TrackingPollSeconds int      `yaml:"tracking_poll_seconds"`
	AcceptRulings       string   `yaml:"accept_rulings"` // always | favorable
	QuoteTTLSeconds     int64    `yaml:"quote_ttl_seconds"`
	PayoutFeeReserve    int64    `yaml:"payout_fee_reserve_sats"`
}

// UpfrontFee is the escrow's fee at funding time.
type UpfrontFee struct {
	BPS     int64  `yaml:"bps" json:"bps"`
	MinSats string `yaml:"min_sats" json:"min_sats"`
	MinUSDC string `yaml:"min_usdc" json:"min_usdc"`
}

// Escrow is the policy of an escrow node.
type Escrow struct {
	UpfrontFee    UpfrontFee `yaml:"upfront_fee"`
	DisputeFeeBPS int64      `yaml:"dispute_fee_bps"`
}

// Load reads and validates a configuration file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(data)
}

// Parse parses and validates configuration YAML, filling defaults.
func Parse(data []byte) (*Config, error) {
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.defaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) defaults() {
	if c.Network == "" {
		c.Network = "ps-lab"
	}
	if c.DataDir == "" {
		c.DataDir = "data"
	}
	if c.Nostr.K <= 0 {
		c.Nostr.K = 2
	}
	if c.P2P.Reachability == "" {
		c.P2P.Reachability = "auto"
	}
	if s := c.Shopper; s != nil {
		if s.Risk.Threshold == 0 {
			s.Risk.Threshold = 70
		}
		if s.Confirmations <= 0 {
			s.Confirmations = 1
		}
		if s.TrackingPollSeconds <= 0 {
			s.TrackingPollSeconds = 60
		}
		if s.AcceptRulings == "" {
			s.AcceptRulings = "always"
		}
		if s.QuoteTTLSeconds <= 0 {
			s.QuoteTTLSeconds = 900
		}
		if s.PayoutFeeReserve <= 0 {
			s.PayoutFeeReserve = 1000
		}
		if s.DeliveryDays <= 0 {
			s.DeliveryDays = 5
		}
		t := &s.Timelock
		// spec §4.5 defaults: t1 = now + (delivery_days + 21) days, t2 = t1 + 14 days (600 s per block)
		if t.EVMT1Seconds <= 0 {
			t.EVMT1Seconds = (s.DeliveryDays + 21) * 86400
		}
		if t.EVMT2Seconds <= 0 {
			t.EVMT2Seconds = t.EVMT1Seconds + 14*86400
		}
		if t.BTCT1Blocks <= 0 {
			t.BTCT1Blocks = t.EVMT1Seconds / 600
		}
		if t.BTCT2Blocks <= 0 {
			t.BTCT2Blocks = t.BTCT1Blocks + 14*144
		}
	}
}

func (c *Config) validate() error {
	switch c.Role {
	case RoleShopper, RoleEscrow, RoleOperator, RoleRelay:
	default:
		return fmt.Errorf("config: unknown role %q", c.Role)
	}
	if c.MnemonicFile == "" {
		return errors.New("config: mnemonic_file is required")
	}
	switch c.P2P.Reachability {
	case "auto", "public", "private":
	default:
		return fmt.Errorf("config: p2p.reachability must be auto, public or private, not %q", c.P2P.Reachability)
	}
	if c.Role == RoleShopper {
		if c.Shopper == nil {
			return errors.New("config: role shopper needs a shopper section")
		}
		if c.Shopper.BotURL == "" {
			return errors.New("config: shopper.bot_url is required")
		}
		switch c.Shopper.AcceptRulings {
		case "always", "favorable":
		default:
			return fmt.Errorf("config: shopper.accept_rulings must be always or favorable, not %q", c.Shopper.AcceptRulings)
		}
		if c.Shopper.Timelock.BTCT1Blocks >= c.Shopper.Timelock.BTCT2Blocks || c.Shopper.Timelock.EVMT1Seconds >= c.Shopper.Timelock.EVMT2Seconds {
			return errors.New("config: shopper timelocks need t1 < t2")
		}
	}
	if c.Role == RoleEscrow && c.Escrow == nil {
		return errors.New("config: role escrow needs an escrow section")
	}
	return nil
}
