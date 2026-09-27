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
		if c.Role == RoleShopper && (c.Shopper.AcceptRulings != "always" || c.Shopper.Timelock.BTCT1Blocks != 100) {
			t.Errorf("%s: shopper section not read: %+v", f, c.Shopper)
		}
	}
	if n == 0 {
		t.Skip("no lab node configs")
	}
}

func containsRole(data []byte) bool {
	return bytes.HasPrefix(data, []byte("role:")) || bytes.Contains(data, []byte("\nrole:"))
}

func TestDefaultsAndValidation(t *testing.T) {
	c, err := Parse([]byte("role: shopper\nmnemonic_file: /k\nshopper: {bot_url: http://bot}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Nostr.K != 2 || c.Shopper.Risk.Threshold != 70 || c.Shopper.AcceptRulings != "always" {
		t.Fatalf("defaults not applied: %+v %+v", c.Nostr, c.Shopper)
	}
	if c.Shopper.Timelock.EVMT1Seconds != 26*86400 || c.Shopper.Timelock.BTCT2Blocks <= c.Shopper.Timelock.BTCT1Blocks {
		t.Fatalf("timelock defaults: %+v", c.Shopper.Timelock)
	}
	for _, bad := range []string{
		"role: nope\nmnemonic_file: /k\n",
		"role: relay\n",
		"role: shopper\nmnemonic_file: /k\n",
		"role: shopper\nmnemonic_file: /k\nshopper: {bot_url: x, accept_rulings: sometimes}\n",
		"role: relay\nmnemonic_file: /k\np2p: {reachability: maybe}\n",
		"role: escrow\nmnemonic_file: /k\n",
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
