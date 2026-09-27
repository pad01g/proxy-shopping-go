package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestLabConfigsParse(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("..", "..", "..", "lab", "nodes", "*.yaml"))
	n := 0
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		// ratefeed.yaml and other non-node files have no role
		if !containsRole(data) {
			continue
		}
		c, err := Parse(data)
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		n++
		if c.Role == RoleShopper && (c.Shopper.AcceptRulings != "always" || c.Shopper.Timelock.BTCT1Blocks != 100 || !c.Shopper.AllowPrivateShops) {
			t.Errorf("%s: shopper section not read: %+v", f, c.Shopper)
		}
	}
	if n == 0 {
		t.Skip("no lab node configs")
	}
}

// coords is a trust section: every role but relay needs coordinators.
const coords = "trust: {coordinators: [6eac25bc912ab49582890fa47837d574e6d7932620d5410fcffec31a8f87d520]}\n"

func containsRole(data []byte) bool {
	return bytes.HasPrefix(data, []byte("role:")) || bytes.Contains(data, []byte("\nrole:"))
}

func TestDefaultsAndValidation(t *testing.T) {
	c, err := Parse([]byte("role: shopper\nmnemonic_file: /k\n" + coords + "shopper: {bot_url: http://bot}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Nostr.K != 2 || c.Shopper.Risk.Threshold != 70 || c.Shopper.AcceptRulings != "always" || c.Shopper.PayoutConfirmations != 3 {
		t.Fatalf("defaults not applied: %+v %+v", c.Nostr, c.Shopper)
	}
	if c.Shopper.Timelock.EVMT1Seconds != 26*86400 || c.Shopper.Timelock.BTCT2Blocks <= c.Shopper.Timelock.BTCT1Blocks {
		t.Fatalf("timelock defaults: %+v", c.Shopper.Timelock)
	}
	if _, err := Parse([]byte("role: shopper\nmnemonic_file: /k\n" + coords + "shopper: {bot_url: x, delivery_days: 85}\n")); err != nil {
		t.Fatalf("85 delivery days: %v", err)
	}
	for _, bad := range []string{
		"role: nope\nmnemonic_file: /k\n",
		"role: relay\n",
		"role: shopper\nmnemonic_file: /k\n",
		"role: shopper\nmnemonic_file: /k\n" + coords + "shopper: {bot_url: x, accept_rulings: sometimes}\n",
		"role: relay\nmnemonic_file: /k\np2p: {reachability: maybe}\n",
		"role: escrow\nmnemonic_file: /k\n",
		// T1 too close to buy, and a payout reserve user clients refuse
		"role: shopper\nmnemonic_file: /k\n" + coords + "shopper: {bot_url: x, timelock: {btc_t1_blocks: 100, btc_t2_blocks: 150, evm_t1_seconds: 3600, evm_t2_seconds: 7200}}\n",
		"role: shopper\nmnemonic_file: /k\n" + coords + "shopper: {bot_url: x, payout_fee_reserve_sats: 20001}\n",
		// the timelocks of 86 delivery days would be beyond the 120 days user clients accept
		"role: shopper\nmnemonic_file: /k\n" + coords + "shopper: {bot_url: x, delivery_days: 86}\n",
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// Every role but relay needs coordinators (§2.4: an empty list trusts nothing), and they must be public keys
// (review 2, item 6).
func TestCoordinatorsRequired(t *testing.T) {
	for _, role := range []string{"operator", "escrow\nescrow: {}", "shopper\nshopper: {bot_url: x}"} {
		if _, err := Parse([]byte("role: " + role + "\nmnemonic_file: /k\n")); err == nil {
			t.Errorf("role %s without coordinators accepted", role)
		}
		if _, err := Parse([]byte("role: " + role + "\nmnemonic_file: /k\n" + coords)); err != nil {
			t.Errorf("role %s: %v", role, err)
		}
	}
	if _, err := Parse([]byte("role: relay\nmnemonic_file: /k\n")); err != nil {
		t.Errorf("relay without coordinators: %v", err)
	}
	if _, err := Parse([]byte("role: operator\nmnemonic_file: /k\ntrust: {coordinators: [npub1xyz]}\n")); err == nil {
		t.Error("a coordinator that is not a hex key accepted")
	}
}
