package evm

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/pad01g/proxy-shopping-go/node/internal/keys"
)

// Deployments is contracts/deployments/<chain_id>.json.
type Deployments struct {
	ChainID int64          `json:"chain_id"`
	USDC    common.Address `json:"usdc"`
	Safe    struct {
		Singleton         common.Address `json:"singleton"`
		Factory           common.Address `json:"factory"`
		FallbackHandler   common.Address `json:"fallback_handler"`
		MultiSendCallOnly common.Address `json:"multisend_call_only"`
	} `json:"safe"`
	Module common.Address            `json:"module"`
	Setup  common.Address            `json:"setup"`
	Feeds  map[string]common.Address `json:"feeds"`
	Bond   common.Address            `json:"bond"`
}

// LoadDeployments reads a deployments file.
func LoadDeployments(path string) (*Deployments, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read deployments: %w", err)
	}
	var d Deployments
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("parse deployments %s: %w", path, err)
	}
	return &d, nil
}

// OrderSafe describes the Safe of one order (§6.2).
type OrderSafe struct {
	User, Shopper, Escrow common.Address
	Token                 common.Address
	Module, Setup         common.Address
	FallbackHandler       common.Address
	T1, T2                uint64 // UNIX seconds
}

// NewOrderSafe fills the contract addresses from the deployments.
func NewOrderSafe(d *Deployments, user, shopper, escrow common.Address, t1, t2 uint64) OrderSafe {
	return OrderSafe{
		User: user, Shopper: shopper, Escrow: escrow,
		Token: d.USDC, Module: d.Module, Setup: d.Setup, FallbackHandler: d.Safe.FallbackHandler,
		T1: t1, T2: t2,
	}
}

// Owners returns [U, S, E].
func (o OrderSafe) Owners() []common.Address { return []common.Address{o.User, o.Shopper, o.Escrow} }

// Initializer is the Safe.setup call of §6.2.
func (o OrderSafe) Initializer() ([]byte, error) {
	if o.T1 == 0 || o.T1 >= o.T2 {
		return nil, fmt.Errorf("invalid timelocks t1=%d t2=%d", o.T1, o.T2)
	}
	inner, err := SetupABI.Pack("setup", o.Module, o.Token, o.User, o.Shopper, o.T1, o.T2)
	if err != nil {
		return nil, fmt.Errorf("pack PSSafeSetup.setup: %w", err)
	}
	var zero common.Address
	data, err := SafeABI.Pack("setup", o.Owners(), big.NewInt(2), o.Setup, inner, o.FallbackHandler, zero, new(big.Int), zero)
	if err != nil {
		return nil, fmt.Errorf("pack Safe.setup: %w", err)
	}
	return data, nil
}

// SaltNonce is uint256(keccak256(order id bytes)).
func SaltNonce(orderID string) (*big.Int, error) {
	raw, err := keys.OrderIDBytes(orderID)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(crypto.Keccak256(raw)), nil
}

// PredictProxy computes the address createProxyWithNonce deploys to.
func PredictProxy(factory, singleton common.Address, proxyCreationCode, initializer []byte, saltNonce *big.Int) common.Address {
	salt := crypto.Keccak256(crypto.Keccak256(initializer), common.LeftPadBytes(saltNonce.Bytes(), 32))
	initCode := append(append([]byte{}, proxyCreationCode...), common.LeftPadBytes(singleton.Bytes(), 32)...)
	return crypto.CreateAddress2(factory, [32]byte(salt), crypto.Keccak256(initCode))
}

// Address predicts the Safe address of the order.
func (o OrderSafe) Address(orderID string, factory, singleton common.Address, proxyCreationCode []byte) (common.Address, error) {
	init, err := o.Initializer()
	if err != nil {
		return common.Address{}, err
	}
	salt, err := SaltNonce(orderID)
	if err != nil {
		return common.Address{}, err
	}
	return PredictProxy(factory, singleton, proxyCreationCode, init, salt), nil
}

// SafeTx is the transaction the owners sign (§6.4).
type SafeTx struct {
	To             common.Address
	Value          *big.Int
	Data           []byte
	Operation      uint8
	SafeTxGas      *big.Int
	BaseGas        *big.Int
	GasPrice       *big.Int
	GasToken       common.Address
	RefundReceiver common.Address
	Nonce          *big.Int
}

var (
	domainTypeHash = crypto.Keccak256Hash([]byte("EIP712Domain(uint256 chainId,address verifyingContract)"))
	safeTxTypeHash = crypto.Keccak256Hash([]byte("SafeTx(address to,uint256 value,bytes data,uint8 operation,uint256 safeTxGas,uint256 baseGas,uint256 gasPrice,address gasToken,address refundReceiver,uint256 nonce)"))
)

func orZero(b *big.Int) *big.Int {
	if b == nil {
		return new(big.Int)
	}
	return b
}

func word(b *big.Int) []byte { return common.LeftPadBytes(orZero(b).Bytes(), 32) }

// Hash is the EIP-712 hash the owners sign.
func (t SafeTx) Hash(chainID int64, safe common.Address) common.Hash {
	domain := crypto.Keccak256(domainTypeHash.Bytes(), word(big.NewInt(chainID)), common.LeftPadBytes(safe.Bytes(), 32))
	structHash := crypto.Keccak256(
		safeTxTypeHash.Bytes(),
		common.LeftPadBytes(t.To.Bytes(), 32),
		word(t.Value),
		crypto.Keccak256(t.Data),
		word(big.NewInt(int64(t.Operation))),
		word(t.SafeTxGas), word(t.BaseGas), word(t.GasPrice),
		common.LeftPadBytes(t.GasToken.Bytes(), 32),
		common.LeftPadBytes(t.RefundReceiver.Bytes(), 32),
		word(t.Nonce),
	)
	return crypto.Keccak256Hash([]byte{0x19, 0x01}, domain, structHash)
}

type safeTxJSON struct {
	To             common.Address  `json:"to"`
	Value          string          `json:"value"`
	Data           string          `json:"data"`
	Operation      json.RawMessage `json:"operation"`
	SafeTxGas      string          `json:"safeTxGas"`
	BaseGas        string          `json:"baseGas"`
	GasPrice       string          `json:"gasPrice"`
	GasToken       common.Address  `json:"gasToken"`
	RefundReceiver common.Address  `json:"refundReceiver"`
	Nonce          string          `json:"nonce"`
}

// MarshalJSON writes numbers as decimal strings and data as 0x hex.
func (t SafeTx) MarshalJSON() ([]byte, error) {
	return json.Marshal(safeTxJSON{
		To: t.To, Value: orZero(t.Value).String(), Data: "0x" + hex.EncodeToString(t.Data),
		Operation: json.RawMessage(strconv.Quote(strconv.Itoa(int(t.Operation)))),
		SafeTxGas: orZero(t.SafeTxGas).String(), BaseGas: orZero(t.BaseGas).String(), GasPrice: orZero(t.GasPrice).String(),
		GasToken: t.GasToken, RefundReceiver: t.RefundReceiver, Nonce: orZero(t.Nonce).String(),
	})
}

// UnmarshalJSON accepts the §6.4 format (operation as a string or a number).
func (t *SafeTx) UnmarshalJSON(data []byte) error {
	var j safeTxJSON
	if err := json.Unmarshal(data, &j); err != nil {
		return err
	}
	num := func(name, s string) (*big.Int, error) {
		if s == "" {
			return new(big.Int), nil
		}
		v, ok := new(big.Int).SetString(s, 10)
		if !ok || v.Sign() < 0 {
			return nil, fmt.Errorf("safe_tx.%s: not a decimal string", name)
		}
		return v, nil
	}
	var err error
	out := SafeTx{To: j.To, GasToken: j.GasToken, RefundReceiver: j.RefundReceiver}
	for _, f := range []struct {
		name, s string
		dst     **big.Int
	}{{"value", j.Value, &out.Value}, {"safeTxGas", j.SafeTxGas, &out.SafeTxGas}, {"baseGas", j.BaseGas, &out.BaseGas},
		{"gasPrice", j.GasPrice, &out.GasPrice}, {"nonce", j.Nonce, &out.Nonce}} {
		if *f.dst, err = num(f.name, f.s); err != nil {
			return err
		}
	}
	if out.Data, err = hex.DecodeString(strings.TrimPrefix(j.Data, "0x")); err != nil {
		return fmt.Errorf("safe_tx.data: %w", err)
	}
	op := strings.Trim(string(j.Operation), `"`)
	if op == "" {
		op = "0"
	}
	n, err := strconv.Atoi(op)
	if err != nil || (n != 0 && n != 1) {
		return fmt.Errorf("safe_tx.operation: %q", op)
	}
	out.Operation = uint8(n)
	*t = out
	return nil
}

// ethKey converts a btcec key into the form go-ethereum signs with. btcec's own
// ToECDSA carries btcec's curve, which the pure-Go (CGO_ENABLED=0) signer rejects.
func ethKey(key *btcec.PrivateKey) (*ecdsa.PrivateKey, error) {
	k, err := crypto.ToECDSA(key.Serialize())
	if err != nil {
		return nil, fmt.Errorf("convert key: %w", err)
	}
	return k, nil
}

// Sign returns the 65 byte signature (r, s, v with v = 27/28).
func Sign(hash common.Hash, key *btcec.PrivateKey) ([]byte, error) {
	k, err := ethKey(key)
	if err != nil {
		return nil, err
	}
	sig, err := crypto.Sign(hash.Bytes(), k)
	if err != nil {
		return nil, fmt.Errorf("sign safe tx: %w", err)
	}
	sig[64] += 27
	return sig, nil
}

// Recover returns the owner that made a 65 byte signature.
func Recover(hash common.Hash, sig []byte) (common.Address, error) {
	if len(sig) != 65 || (sig[64] != 27 && sig[64] != 28) {
		return common.Address{}, errors.New("expected a 65 byte signature with v 27/28")
	}
	s := append([]byte{}, sig...)
	s[64] -= 27
	pub, err := crypto.SigToPub(hash.Bytes(), s)
	if err != nil {
		return common.Address{}, fmt.Errorf("recover signer: %w", err)
	}
	return crypto.PubkeyToAddress(*pub), nil
}

// JoinSignatures concatenates signatures ordered by the signer address, as execTransaction wants them.
func JoinSignatures(sigs map[common.Address][]byte) []byte {
	owners := make([]common.Address, 0, len(sigs))
	for a := range sigs {
		owners = append(owners, a)
	}
	sort.Slice(owners, func(i, j int) bool { return bytes.Compare(owners[i].Bytes(), owners[j].Bytes()) < 0 })
	var out []byte
	for _, a := range owners {
		out = append(out, sigs[a]...)
	}
	return out
}

// Transfer is one ERC-20 payment of a ruling.
type Transfer struct {
	To     common.Address
	Amount *big.Int
}

// ReleaseTx pays the whole lock to the shopper (§6.4).
func ReleaseTx(token, shopper common.Address, amount *big.Int, nonce *big.Int) (SafeTx, error) {
	data, err := ERC20ABI.Pack("transfer", shopper, amount)
	if err != nil {
		return SafeTx{}, err
	}
	return SafeTx{To: token, Value: new(big.Int), Data: data, Nonce: orZero(nonce)}, nil
}

// SplitTx pays several parties through MultiSendCallOnly (delegatecall). Zero amounts are left out.
func SplitTx(multiSend, token common.Address, transfers []Transfer, nonce *big.Int) (SafeTx, error) {
	var packed []byte
	for _, tr := range transfers {
		if tr.Amount == nil || tr.Amount.Sign() <= 0 {
			continue
		}
		call, err := ERC20ABI.Pack("transfer", tr.To, tr.Amount)
		if err != nil {
			return SafeTx{}, err
		}
		packed = append(packed, 0) // operation: call
		packed = append(packed, token.Bytes()...)
		packed = append(packed, make([]byte, 32)...) // value
		packed = append(packed, common.LeftPadBytes(big.NewInt(int64(len(call))).Bytes(), 32)...)
		packed = append(packed, call...)
	}
	if len(packed) == 0 {
		return SafeTx{}, errors.New("split without payments")
	}
	data, err := MultiSendABI.Pack("multiSend", packed)
	if err != nil {
		return SafeTx{}, err
	}
	return SafeTx{To: multiSend, Value: new(big.Int), Data: data, Operation: 1, Nonce: orZero(nonce)}, nil
}

// DecodeTransfers lists the ERC-20 transfers of a release or split SafeTx.
func DecodeTransfers(t SafeTx, token, multiSend common.Address) ([]Transfer, error) {
	decodeTransfer := func(data []byte) (Transfer, error) {
		if len(data) < 4 || !bytes.Equal(data[:4], ERC20ABI.Methods["transfer"].ID) {
			return Transfer{}, errors.New("not an ERC-20 transfer")
		}
		v, err := ERC20ABI.Methods["transfer"].Inputs.Unpack(data[4:])
		if err != nil {
			return Transfer{}, err
		}
		return Transfer{To: v[0].(common.Address), Amount: v[1].(*big.Int)}, nil
	}
	if t.Value != nil && t.Value.Sign() != 0 {
		return nil, errors.New("safe tx sends ether")
	}
	switch {
	case t.Operation == 0 && t.To == token:
		tr, err := decodeTransfer(t.Data)
		if err != nil {
			return nil, err
		}
		return []Transfer{tr}, nil
	case t.Operation == 1 && t.To == multiSend:
		if len(t.Data) < 4 || !bytes.Equal(t.Data[:4], MultiSendABI.Methods["multiSend"].ID) {
			return nil, errors.New("not a multiSend call")
		}
		v, err := MultiSendABI.Methods["multiSend"].Inputs.Unpack(t.Data[4:])
		if err != nil {
			return nil, err
		}
		packed := v[0].([]byte)
		var out []Transfer
		for len(packed) > 0 {
			if len(packed) < 85 {
				return nil, errors.New("truncated multiSend entry")
			}
			op, to := packed[0], common.BytesToAddress(packed[1:21])
			value := new(big.Int).SetBytes(packed[21:53])
			n := new(big.Int).SetBytes(packed[53:85])
			if !n.IsInt64() || int64(len(packed)-85) < n.Int64() {
				return nil, errors.New("truncated multiSend data")
			}
			if op != 0 || to != token || value.Sign() != 0 {
				return nil, errors.New("multiSend entry is not a plain token transfer")
			}
			tr, err := decodeTransfer(packed[85 : 85+n.Int64()])
			if err != nil {
				return nil, err
			}
			out = append(out, tr)
			packed = packed[85+n.Int64():]
		}
		return out, nil
	}
	return nil, errors.New("safe tx is neither a token transfer nor a multiSend of transfers")
}
