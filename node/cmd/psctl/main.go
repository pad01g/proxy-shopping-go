// psctl signs the trust events of coordinators and operators and prints the keys of a mnemonic.
//
//	psctl keys --mnemonic-file m
//	psctl delegate --mnemonic-file m --operator <pk> --version N [--revoke] [--note text]
//	psctl list --mnemonic-file m --file list.json --version N
//	psctl profile --mnemonic-file m --kind shopper|escrow --file content.json --version N
//	psctl inbox --mnemonic-file m --relay wss://a --relay wss://b --version N
//
// Signing commands print the event; --publish wss://…,wss://… sends it to relays and --node http://host:8080
// (with --token) posts it to a psnode.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"

	"github.com/pad01g/proxy-shopping-go/node/internal/httpx"
	"github.com/pad01g/proxy-shopping-go/node/internal/keys"
	"github.com/pad01g/proxy-shopping-go/node/internal/nostrnet"
	"github.com/pad01g/proxy-shopping-go/node/internal/trust"
)

const usage = `usage: psctl <keys|delegate|list|profile|inbox> [flags]
run "psctl <command> -h" for the flags of a command`

type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error {
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			*l = append(*l, s)
		}
	}
	return nil
}

// common are the flags of the signing commands.
type common struct {
	mnemonic string
	network  string
	version  int64
	publish  listFlag
	node     string
	token    string
	extraCA  string
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.mnemonic, "mnemonic-file", "", "BIP39 mnemonic of the signer")
	fs.StringVar(&c.network, "network", "ps-lab", "network name")
	fs.Int64Var(&c.version, "version", 0, "version (v tag); 0 means the current UNIX time")
	fs.Var(&c.publish, "publish", "relay URLs to publish to (comma separated or repeated)")
	fs.StringVar(&c.node, "node", "", "psnode admin URL to POST the event to")
	fs.StringVar(&c.token, "token", os.Getenv("PS_ADMIN_TOKEN"), "psnode admin token")
	fs.StringVar(&c.extraCA, "extra-ca", os.Getenv("PS_EXTRA_CA"), "extra CA certificate (PEM) for wss/https")
}

func (c *common) keys() (*keys.Set, error) {
	if c.mnemonic == "" {
		return nil, errors.New("--mnemonic-file is required")
	}
	return keys.LoadMnemonicFile(c.mnemonic)
}

func (c *common) v() int64 {
	if c.version > 0 {
		return c.version
	}
	return time.Now().Unix()
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "keys":
		err = cmdKeys(os.Args[2:])
	case "delegate":
		err = cmdDelegate(os.Args[2:])
	case "list":
		err = cmdList(os.Args[2:])
	case "profile":
		err = cmdProfile(os.Args[2:])
	case "inbox":
		err = cmdInbox(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Println(usage)
	default:
		err = fmt.Errorf("unknown command %q\n%s", os.Args[1], usage)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "psctl:", err)
		os.Exit(1)
	}
}

// KeysOutput is what psctl keys prints.
type KeysOutput struct {
	NostrPubkey   string `json:"nostr_pubkey"`
	Npub          string `json:"npub"`
	EVMAddress    string `json:"evm_address"`
	BTCAddress    string `json:"btc_address"`
	BTCEscrowXpub string `json:"btc_escrow_xpub"`
	Libp2pPeerID  string `json:"libp2p_peer_id"`
}

func keysOutput(s *keys.Set) (KeysOutput, error) {
	xpub, err := s.EscrowXpub()
	if err != nil {
		return KeysOutput{}, err
	}
	id, err := s.PeerID()
	if err != nil {
		return KeysOutput{}, err
	}
	npub, err := nip19.EncodePublicKey(s.NostrPubHex())
	if err != nil {
		return KeysOutput{}, err
	}
	return KeysOutput{
		NostrPubkey: s.NostrPubHex(), Npub: npub, EVMAddress: s.EVMAddress().Hex(), BTCAddress: s.WalletAddress(),
		BTCEscrowXpub: xpub, Libp2pPeerID: id.String(),
	}, nil
}

func cmdKeys(args []string) error {
	fs := flag.NewFlagSet("keys", flag.ExitOnError)
	file := fs.String("mnemonic-file", "", "BIP39 mnemonic")
	_ = fs.Parse(args)
	if *file == "" {
		return errors.New("--mnemonic-file is required")
	}
	s, err := keys.LoadMnemonicFile(*file)
	if err != nil {
		return err
	}
	out, err := keysOutput(s)
	if err != nil {
		return err
	}
	return printJSON(out)
}

func cmdDelegate(args []string) error {
	fs := flag.NewFlagSet("delegate", flag.ExitOnError)
	var c common
	c.register(fs)
	op := fs.String("operator", "", "operator pubkey (hex or npub)")
	revoke := fs.Bool("revoke", false, "revoke the delegation")
	note := fs.String("note", "", "note")
	_ = fs.Parse(args)
	s, err := c.keys()
	if err != nil {
		return err
	}
	pk, err := pubkey(*op)
	if err != nil {
		return err
	}
	ev, err := trust.NewDelegation(s.NostrSecretHex(), pk, c.network, c.v(), *revoke, *note)
	if err != nil {
		return err
	}
	return c.emit(ev)
}

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	var c common
	c.register(fs)
	file := fs.String("file", "", "list content (JSON of spec §2.3)")
	_ = fs.Parse(args)
	s, err := c.keys()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	var l trust.List
	if err := json.Unmarshal(data, &l); err != nil {
		return fmt.Errorf("%s: %w", *file, err)
	}
	if l.Network == "" {
		l.Network = c.network
	}
	if l.ReportTo == "" {
		l.ReportTo = s.NostrPubHex()
	}
	ev, err := trust.NewList(s.NostrSecretHex(), c.v(), &l)
	if err != nil {
		return err
	}
	return c.emit(ev)
}

func cmdProfile(args []string) error {
	fs := flag.NewFlagSet("profile", flag.ExitOnError)
	var c common
	c.register(fs)
	kind := fs.String("kind", "", "shopper or escrow")
	file := fs.String("file", "", "profile content (JSON of spec §3)")
	_ = fs.Parse(args)
	s, err := c.keys()
	if err != nil {
		return err
	}
	k := map[string]int{"shopper": trust.KindShopperProfile, "escrow": trust.KindEscrowProfile}[*kind]
	if k == 0 {
		return errors.New("--kind must be shopper or escrow")
	}
	data, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	if !json.Valid(data) {
		return fmt.Errorf("%s is not JSON", *file)
	}
	var compact bytes.Buffer
	_ = json.Compact(&compact, data)
	ev, err := trust.NewProfile(s.NostrSecretHex(), k, c.network, c.v(), compact.String())
	if err != nil {
		return err
	}
	return c.emit(ev)
}

func cmdInbox(args []string) error {
	fs := flag.NewFlagSet("inbox", flag.ExitOnError)
	var c common
	c.register(fs)
	var relays listFlag
	fs.Var(&relays, "relay", "inbox relay URL (repeat)")
	_ = fs.Parse(args)
	s, err := c.keys()
	if err != nil {
		return err
	}
	if len(relays) == 0 {
		return errors.New("at least one --relay is required")
	}
	ev, err := trust.NewInboxRelays(s.NostrSecretHex(), relays, c.v())
	if err != nil {
		return err
	}
	return c.emit(ev)
}

func pubkey(s string) (string, error) {
	if strings.HasPrefix(s, "npub") {
		_, v, err := nip19.Decode(s)
		if err != nil {
			return "", err
		}
		return v.(string), nil
	}
	if !nostr.IsValidPublicKey(s) {
		return "", fmt.Errorf("%q is not a public key", s)
	}
	return s, nil
}

// emit prints the event and publishes it where asked.
func (c *common) emit(ev *nostr.Event) error {
	if err := printJSON(ev); err != nil {
		return err
	}
	if len(c.publish) == 0 && c.node == "" {
		return nil
	}
	tc, err := httpx.TLSConfig(c.extraCA)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if len(c.publish) > 0 {
		pool := nostrnet.NewPool(tc, nil)
		defer pool.Close()
		ok, err := pool.Publish(ctx, c.publish, ev)
		fmt.Fprintf(os.Stderr, "published to %d/%d relays\n", len(ok), len(c.publish))
		if len(ok) == 0 {
			return fmt.Errorf("publish: %w", err)
		}
	}
	if c.node != "" {
		body, _ := json.Marshal(ev)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.node, "/")+"/events", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		res, err := httpx.Client(tc, 30*time.Second).Do(req)
		if err != nil {
			return fmt.Errorf("post to node: %w", err)
		}
		defer res.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
		if res.StatusCode/100 != 2 {
			return fmt.Errorf("node answered %d: %s", res.StatusCode, strings.TrimSpace(string(msg)))
		}
		fmt.Fprintf(os.Stderr, "node: %s\n", strings.TrimSpace(string(msg)))
	}
	return nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
