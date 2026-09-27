// Package evm implements the USDC escrow of spec §6: per order Safe v1.4.1 proxies, SafeTx signatures and the
// PSEscrowModule timelock.
package evm

import (
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
)

// Minimal hand-written ABIs of the functions the node calls.
const (
	safeABIJSON = `[
{"type":"function","name":"setup","stateMutability":"nonpayable","inputs":[
 {"name":"_owners","type":"address[]"},{"name":"_threshold","type":"uint256"},{"name":"to","type":"address"},
 {"name":"data","type":"bytes"},{"name":"fallbackHandler","type":"address"},{"name":"paymentToken","type":"address"},
 {"name":"payment","type":"uint256"},{"name":"paymentReceiver","type":"address"}],"outputs":[]},
{"type":"function","name":"execTransaction","stateMutability":"payable","inputs":[
 {"name":"to","type":"address"},{"name":"value","type":"uint256"},{"name":"data","type":"bytes"},
 {"name":"operation","type":"uint8"},{"name":"safeTxGas","type":"uint256"},{"name":"baseGas","type":"uint256"},
 {"name":"gasPrice","type":"uint256"},{"name":"gasToken","type":"address"},{"name":"refundReceiver","type":"address"},
 {"name":"signatures","type":"bytes"}],"outputs":[{"name":"success","type":"bool"}]},
{"type":"function","name":"getTransactionHash","stateMutability":"view","inputs":[
 {"name":"to","type":"address"},{"name":"value","type":"uint256"},{"name":"data","type":"bytes"},
 {"name":"operation","type":"uint8"},{"name":"safeTxGas","type":"uint256"},{"name":"baseGas","type":"uint256"},
 {"name":"gasPrice","type":"uint256"},{"name":"gasToken","type":"address"},{"name":"refundReceiver","type":"address"},
 {"name":"_nonce","type":"uint256"}],"outputs":[{"name":"","type":"bytes32"}]},
{"type":"function","name":"nonce","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"uint256"}]},
{"type":"function","name":"getOwners","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"address[]"}]},
{"type":"function","name":"getThreshold","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"uint256"}]},
{"type":"function","name":"isModuleEnabled","stateMutability":"view","inputs":[{"name":"module","type":"address"}],"outputs":[{"name":"","type":"bool"}]}
]`
	factoryABIJSON = `[
{"type":"function","name":"createProxyWithNonce","stateMutability":"nonpayable","inputs":[
 {"name":"_singleton","type":"address"},{"name":"initializer","type":"bytes"},{"name":"saltNonce","type":"uint256"}],
 "outputs":[{"name":"proxy","type":"address"}]},
{"type":"function","name":"proxyCreationCode","stateMutability":"pure","inputs":[],"outputs":[{"name":"","type":"bytes"}]}
]`
	erc20ABIJSON = `[
{"type":"function","name":"transfer","stateMutability":"nonpayable","inputs":[{"name":"to","type":"address"},{"name":"value","type":"uint256"}],"outputs":[{"name":"","type":"bool"}]},
{"type":"function","name":"balanceOf","stateMutability":"view","inputs":[{"name":"owner","type":"address"}],"outputs":[{"name":"","type":"uint256"}]},
{"type":"function","name":"mint","stateMutability":"nonpayable","inputs":[{"name":"to","type":"address"},{"name":"value","type":"uint256"}],"outputs":[]},
{"type":"event","name":"Transfer","anonymous":false,"inputs":[{"name":"from","type":"address","indexed":true},{"name":"to","type":"address","indexed":true},{"name":"value","type":"uint256","indexed":false}]}
]`
	multiSendABIJSON = `[
{"type":"function","name":"multiSend","stateMutability":"payable","inputs":[{"name":"transactions","type":"bytes"}],"outputs":[]}
]`
	moduleABIJSON = `[
{"type":"function","name":"claimByShopper","stateMutability":"nonpayable","inputs":[{"name":"safe","type":"address"}],"outputs":[]},
{"type":"function","name":"refundToUser","stateMutability":"nonpayable","inputs":[{"name":"safe","type":"address"}],"outputs":[]},
{"type":"function","name":"config","stateMutability":"view","inputs":[{"name":"safe","type":"address"}],"outputs":[
 {"name":"token","type":"address"},{"name":"user","type":"address"},{"name":"shopper","type":"address"},{"name":"t1","type":"uint64"},{"name":"t2","type":"uint64"}]}
]`
	setupABIJSON = `[
{"type":"function","name":"setup","stateMutability":"nonpayable","inputs":[
 {"name":"module","type":"address"},{"name":"token","type":"address"},{"name":"user","type":"address"},
 {"name":"shopper","type":"address"},{"name":"t1","type":"uint64"},{"name":"t2","type":"uint64"}],"outputs":[]}
]`
	aggregatorABIJSON = `[
{"type":"function","name":"latestRoundData","stateMutability":"view","inputs":[],"outputs":[
 {"name":"roundId","type":"uint80"},{"name":"answer","type":"int256"},{"name":"startedAt","type":"uint256"},
 {"name":"updatedAt","type":"uint256"},{"name":"answeredInRound","type":"uint80"}]},
{"type":"function","name":"decimals","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"uint8"}]},
{"type":"function","name":"setAnswer","stateMutability":"nonpayable","inputs":[{"name":"answer","type":"int256"}],"outputs":[]}
]`
)

// Parsed ABIs.
var (
	SafeABI       = mustABI(safeABIJSON)
	FactoryABI    = mustABI(factoryABIJSON)
	ERC20ABI      = mustABI(erc20ABIJSON)
	MultiSendABI  = mustABI(multiSendABIJSON)
	ModuleABI     = mustABI(moduleABIJSON)
	SetupABI      = mustABI(setupABIJSON)
	AggregatorABI = mustABI(aggregatorABIJSON)
)

func mustABI(s string) abi.ABI {
	a, err := abi.JSON(strings.NewReader(s))
	if err != nil {
		panic(err)
	}
	return a
}
