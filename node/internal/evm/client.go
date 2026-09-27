package evm

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// Client wraps an EVM JSON-RPC endpoint with the calls of the escrow.
type Client struct {
	Eth     *ethclient.Client
	RPC     *rpc.Client
	ChainID *big.Int

	mu        sync.Mutex
	proxyCode map[common.Address][]byte
}

// Dial connects to an RPC endpoint; hc may carry a custom TLS configuration.
func Dial(ctx context.Context, url string, hc *http.Client) (*Client, error) {
	var opts []rpc.ClientOption
	if hc != nil {
		opts = append(opts, rpc.WithHTTPClient(hc))
	}
	rc, err := rpc.DialOptions(ctx, url, opts...)
	if err != nil {
		return nil, fmt.Errorf("dial evm %s: %w", url, err)
	}
	c := &Client{Eth: ethclient.NewClient(rc), RPC: rc, proxyCode: map[common.Address][]byte{}}
	return c, nil
}

// chainID asks the node once.
func (c *Client) chainID(ctx context.Context) (*big.Int, error) {
	c.mu.Lock()
	id := c.ChainID
	c.mu.Unlock()
	if id != nil {
		return id, nil
	}
	id, err := c.Eth.ChainID(ctx)
	if err != nil {
		return nil, fmt.Errorf("chain id: %w", err)
	}
	c.mu.Lock()
	c.ChainID = id
	c.mu.Unlock()
	return id, nil
}

// Call runs a view function and unpacks its outputs.
func (c *Client) Call(ctx context.Context, contract common.Address, a abi.ABI, method string, args ...any) ([]any, error) {
	data, err := a.Pack(method, args...)
	if err != nil {
		return nil, fmt.Errorf("pack %s: %w", method, err)
	}
	out, err := c.Eth.CallContract(ctx, ethereum.CallMsg{To: &contract, Data: data}, nil)
	if err != nil {
		return nil, fmt.Errorf("call %s on %s: %w", method, contract, err)
	}
	vals, err := a.Unpack(method, out)
	if err != nil {
		return nil, fmt.Errorf("unpack %s: %w", method, err)
	}
	return vals, nil
}

// ProxyCreationCode reads (and caches) SafeProxyFactory.proxyCreationCode().
func (c *Client) ProxyCreationCode(ctx context.Context, factory common.Address) ([]byte, error) {
	c.mu.Lock()
	code, ok := c.proxyCode[factory]
	c.mu.Unlock()
	if ok {
		return code, nil
	}
	v, err := c.Call(ctx, factory, FactoryABI, "proxyCreationCode")
	if err != nil {
		return nil, err
	}
	code = v[0].([]byte)
	c.mu.Lock()
	c.proxyCode[factory] = code
	c.mu.Unlock()
	return code, nil
}

// PredictSafe computes the order's Safe address with the factory of the deployments.
func (c *Client) PredictSafe(ctx context.Context, d *Deployments, o OrderSafe, orderID string) (common.Address, error) {
	code, err := c.ProxyCreationCode(ctx, d.Safe.Factory)
	if err != nil {
		return common.Address{}, err
	}
	return o.Address(orderID, d.Safe.Factory, d.Safe.Singleton, code)
}

// BalanceOf is ERC20.balanceOf.
func (c *Client) BalanceOf(ctx context.Context, token, owner common.Address) (*big.Int, error) {
	v, err := c.Call(ctx, token, ERC20ABI, "balanceOf", owner)
	if err != nil {
		return nil, err
	}
	return v[0].(*big.Int), nil
}

// SafeNonce is Safe.nonce().
func (c *Client) SafeNonce(ctx context.Context, safe common.Address) (*big.Int, error) {
	v, err := c.Call(ctx, safe, SafeABI, "nonce")
	if err != nil {
		return nil, err
	}
	return v[0].(*big.Int), nil
}

// SafeState is what the shopper checks before it buys.
type SafeState struct {
	Owners        []common.Address
	Threshold     *big.Int
	ModuleEnabled bool
	Config        ModuleConfig
}

// ModuleConfig is PSEscrowModule.config(safe).
type ModuleConfig struct {
	Token, User, Shopper common.Address
	T1, T2               uint64
}

// InspectSafe reads owners, threshold and the module registration of a deployed Safe.
func (c *Client) InspectSafe(ctx context.Context, safe, module common.Address) (*SafeState, error) {
	code, err := c.Eth.CodeAt(ctx, safe, nil)
	if err != nil {
		return nil, fmt.Errorf("code of %s: %w", safe, err)
	}
	if len(code) == 0 {
		return nil, fmt.Errorf("no contract at %s", safe)
	}
	st := &SafeState{}
	v, err := c.Call(ctx, safe, SafeABI, "getOwners")
	if err != nil {
		return nil, err
	}
	st.Owners = v[0].([]common.Address)
	if v, err = c.Call(ctx, safe, SafeABI, "getThreshold"); err != nil {
		return nil, err
	}
	st.Threshold = v[0].(*big.Int)
	if v, err = c.Call(ctx, safe, SafeABI, "isModuleEnabled", module); err != nil {
		return nil, err
	}
	st.ModuleEnabled = v[0].(bool)
	if v, err = c.Call(ctx, module, ModuleABI, "config", safe); err != nil {
		return nil, err
	}
	st.Config = ModuleConfig{Token: v[0].(common.Address), User: v[1].(common.Address), Shopper: v[2].(common.Address), T1: v[3].(uint64), T2: v[4].(uint64)}
	return st, nil
}

// LatestTime is the timestamp of the latest block.
func (c *Client) LatestTime(ctx context.Context) (uint64, error) {
	h, err := c.Eth.HeaderByNumber(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("latest header: %w", err)
	}
	return h.Time, nil
}

// Send signs and sends a contract call from key; it returns the hash without waiting.
func (c *Client) Send(ctx context.Context, key *btcec.PrivateKey, to common.Address, data []byte, value *big.Int) (common.Hash, error) {
	chainID, err := c.chainID(ctx)
	if err != nil {
		return common.Hash{}, err
	}
	from := crypto.PubkeyToAddress(*key.PubKey().ToECDSA())
	nonce, err := c.Eth.PendingNonceAt(ctx, from)
	if err != nil {
		return common.Hash{}, fmt.Errorf("nonce of %s: %w", from, err)
	}
	if value == nil {
		value = new(big.Int)
	}
	gas, err := c.Eth.EstimateGas(ctx, ethereum.CallMsg{From: from, To: &to, Data: data, Value: value})
	if err != nil {
		return common.Hash{}, fmt.Errorf("estimate gas: %w", err)
	}
	tip, err := c.Eth.SuggestGasTipCap(ctx)
	if err != nil {
		return common.Hash{}, fmt.Errorf("gas tip: %w", err)
	}
	head, err := c.Eth.HeaderByNumber(ctx, nil)
	if err != nil {
		return common.Hash{}, fmt.Errorf("latest header: %w", err)
	}
	feeCap := new(big.Int).Add(tip, new(big.Int).Mul(orZero(head.BaseFee), big.NewInt(2)))
	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID: chainID, Nonce: nonce, GasTipCap: tip, GasFeeCap: feeCap,
		Gas: gas + gas/5, To: &to, Value: value, Data: data,
	})
	ek, err := ethKey(key)
	if err != nil {
		return common.Hash{}, err
	}
	signed, err := types.SignTx(tx, types.LatestSignerForChainID(chainID), ek)
	if err != nil {
		return common.Hash{}, fmt.Errorf("sign tx: %w", err)
	}
	if err := c.Eth.SendTransaction(ctx, signed); err != nil {
		return common.Hash{}, fmt.Errorf("send tx: %w", err)
	}
	return signed.Hash(), nil
}

// Wait waits for the receipt of a transaction and fails if it reverted.
func (c *Client) Wait(ctx context.Context, hash common.Hash) (*types.Receipt, error) {
	for {
		r, err := c.Eth.TransactionReceipt(ctx, hash)
		if err == nil {
			if r.Status != types.ReceiptStatusSuccessful {
				return r, fmt.Errorf("transaction %s reverted", hash)
			}
			return r, nil
		}
		if !errors.Is(err, ethereum.NotFound) {
			return nil, fmt.Errorf("receipt of %s: %w", hash, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// SendAndWait sends a call and waits for it to be mined.
func (c *Client) SendAndWait(ctx context.Context, key *btcec.PrivateKey, to common.Address, data []byte) (*types.Receipt, error) {
	h, err := c.Send(ctx, key, to, data, nil)
	if err != nil {
		return nil, err
	}
	return c.Wait(ctx, h)
}

// ExecTransaction runs a SafeTx with the owners' signatures (any order; they are sorted here).
func (c *Client) ExecTransaction(ctx context.Context, key *btcec.PrivateKey, safe common.Address, t SafeTx, sigs map[common.Address][]byte) (*types.Receipt, error) {
	data, err := SafeABI.Pack("execTransaction", t.To, orZero(t.Value), t.Data, t.Operation,
		orZero(t.SafeTxGas), orZero(t.BaseGas), orZero(t.GasPrice), t.GasToken, t.RefundReceiver, JoinSignatures(sigs))
	if err != nil {
		return nil, fmt.Errorf("pack execTransaction: %w", err)
	}
	return c.SendAndWait(ctx, key, safe, data)
}

// DeploySafe calls createProxyWithNonce for the order (usually done by the user).
func (c *Client) DeploySafe(ctx context.Context, key *btcec.PrivateKey, d *Deployments, o OrderSafe, orderID string) (*types.Receipt, error) {
	init, err := o.Initializer()
	if err != nil {
		return nil, err
	}
	salt, err := SaltNonce(orderID)
	if err != nil {
		return nil, err
	}
	data, err := FactoryABI.Pack("createProxyWithNonce", d.Safe.Singleton, init, salt)
	if err != nil {
		return nil, err
	}
	return c.SendAndWait(ctx, key, d.Safe.Factory, data)
}

// TransferToken sends an ERC-20 transfer.
func (c *Client) TransferToken(ctx context.Context, key *btcec.PrivateKey, token, to common.Address, amount *big.Int) (*types.Receipt, error) {
	data, err := ERC20ABI.Pack("transfer", to, amount)
	if err != nil {
		return nil, err
	}
	return c.SendAndWait(ctx, key, token, data)
}

// ClaimByShopper calls PSEscrowModule.claimByShopper(safe).
func (c *Client) ClaimByShopper(ctx context.Context, key *btcec.PrivateKey, module, safe common.Address) (*types.Receipt, error) {
	data, err := ModuleABI.Pack("claimByShopper", safe)
	if err != nil {
		return nil, err
	}
	return c.SendAndWait(ctx, key, module, data)
}

// RefundToUser calls PSEscrowModule.refundToUser(safe).
func (c *Client) RefundToUser(ctx context.Context, key *btcec.PrivateKey, module, safe common.Address) (*types.Receipt, error) {
	data, err := ModuleABI.Pack("refundToUser", safe)
	if err != nil {
		return nil, err
	}
	return c.SendAndWait(ctx, key, module, data)
}

// TokenTransfers returns the ERC-20 Transfer events of token in a mined transaction.
func (c *Client) TokenTransfers(ctx context.Context, txHash common.Hash, token common.Address) ([]TransferEvent, error) {
	r, err := c.Eth.TransactionReceipt(ctx, txHash)
	if err != nil {
		return nil, fmt.Errorf("receipt of %s: %w", txHash, err)
	}
	if r.Status != types.ReceiptStatusSuccessful {
		return nil, fmt.Errorf("transaction %s reverted", txHash)
	}
	topic := ERC20ABI.Events["Transfer"].ID
	var out []TransferEvent
	for _, l := range r.Logs {
		if l.Address != token || len(l.Topics) != 3 || l.Topics[0] != topic {
			continue
		}
		out = append(out, TransferEvent{
			From:   common.BytesToAddress(l.Topics[1].Bytes()),
			To:     common.BytesToAddress(l.Topics[2].Bytes()),
			Amount: new(big.Int).SetBytes(l.Data),
		})
	}
	return out, nil
}

// TransferEvent is an ERC-20 Transfer log.
type TransferEvent struct {
	From, To common.Address
	Amount   *big.Int
}

// Deploy sends a contract creation and returns the new contract's address.
func (c *Client) Deploy(ctx context.Context, key *btcec.PrivateKey, code []byte) (common.Address, error) {
	chainID, err := c.chainID(ctx)
	if err != nil {
		return common.Address{}, err
	}
	from := crypto.PubkeyToAddress(*key.PubKey().ToECDSA())
	nonce, err := c.Eth.PendingNonceAt(ctx, from)
	if err != nil {
		return common.Address{}, err
	}
	gas, err := c.Eth.EstimateGas(ctx, ethereum.CallMsg{From: from, Data: code})
	if err != nil {
		return common.Address{}, fmt.Errorf("estimate deploy gas: %w", err)
	}
	price, err := c.Eth.SuggestGasPrice(ctx)
	if err != nil {
		return common.Address{}, err
	}
	tx := types.NewTx(&types.LegacyTx{Nonce: nonce, GasPrice: price, Gas: gas + gas/5, Data: code})
	ek, err := ethKey(key)
	if err != nil {
		return common.Address{}, err
	}
	signed, err := types.SignTx(tx, types.LatestSignerForChainID(chainID), ek)
	if err != nil {
		return common.Address{}, err
	}
	if err := c.Eth.SendTransaction(ctx, signed); err != nil {
		return common.Address{}, fmt.Errorf("send deploy: %w", err)
	}
	r, err := c.Wait(ctx, signed.Hash())
	if err != nil {
		return common.Address{}, err
	}
	return r.ContractAddress, nil
}
