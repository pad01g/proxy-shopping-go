//go:build integration

package testutil

import (
	"context"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/ethereum/go-ethereum/common"

	"github.com/pad01g/proxy-shopping-go/node/internal/evm"
)

// AnvilKey is anvil's default account 0.
const AnvilKey = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"

// contractsOut finds contracts/out above the working directory.
func contractsOut() (string, bool) {
	dir, _ := os.Getwd()
	for i := 0; i < 6; i++ {
		p := filepath.Join(dir, "contracts", "out")
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return p, true
		}
		dir = filepath.Dir(dir)
	}
	return "", false
}

func bytecode(t testing.TB, out, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(out, name+".sol", name+".json"))
	if err != nil {
		t.Skipf("contract artifact %s missing (build contracts/ with forge first): %v", name, err)
	}
	var art struct {
		Bytecode struct {
			Object string `json:"object"`
		} `json:"bytecode"`
	}
	if err := json.Unmarshal(data, &art); err != nil {
		t.Fatal(err)
	}
	return common.FromHex(art.Bytecode.Object)
}

// DeployLab deploys Safe v1.4.1, MockUSDC, PSEscrowModule and PSSafeSetup from the forge artifacts of
// contracts/out. The test is skipped when the artifacts are missing.
func DeployLab(t testing.TB, ctx context.Context, c *evm.Client) *evm.Deployments {
	t.Helper()
	out, ok := contractsOut()
	if !ok {
		t.Skip("contracts/out not found; build contracts/ with forge to run the EVM integration tests")
	}
	key, _ := btcec.PrivKeyFromBytes(common.FromHex(AnvilKey))
	deploy := func(name string) common.Address {
		addr, err := c.Deploy(ctx, key, bytecode(t, out, name))
		if err != nil {
			t.Fatalf("deploy %s: %v", name, err)
		}
		return addr
	}
	d := &evm.Deployments{ChainID: 31337}
	d.Safe.Singleton = deploy("SafeL2")
	d.Safe.Factory = deploy("SafeProxyFactory")
	d.Safe.FallbackHandler = deploy("CompatibilityFallbackHandler")
	d.Safe.MultiSendCallOnly = deploy("MultiSendCallOnly")
	d.USDC = deploy("MockUSDC")
	d.Module = deploy("PSEscrowModule")
	d.Setup = deploy("PSSafeSetup")
	return d
}

// Fund gives an account ether (anvil_setBalance) and mints it USDC units.
func Fund(t testing.TB, ctx context.Context, c *evm.Client, d *evm.Deployments, to common.Address, usdc int64) {
	t.Helper()
	wei := new(big.Int).Mul(big.NewInt(10), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
	if err := c.RPC.CallContext(ctx, nil, "anvil_setBalance", to, "0x"+wei.Text(16)); err != nil {
		t.Fatal(err)
	}
	if usdc > 0 {
		key, _ := btcec.PrivKeyFromBytes(common.FromHex(AnvilKey))
		data, _ := evm.ERC20ABI.Pack("mint", to, big.NewInt(usdc))
		if _, err := c.SendAndWait(ctx, key, d.USDC, data); err != nil {
			t.Fatal(err)
		}
	}
}

// AdvanceTime moves anvil's clock and mines a block.
func AdvanceTime(t testing.TB, ctx context.Context, c *evm.Client, seconds int64) {
	t.Helper()
	if err := c.RPC.CallContext(ctx, nil, "evm_increaseTime", seconds); err != nil {
		t.Fatal(err)
	}
	if err := c.RPC.CallContext(ctx, nil, "evm_mine"); err != nil {
		t.Fatal(err)
	}
}
