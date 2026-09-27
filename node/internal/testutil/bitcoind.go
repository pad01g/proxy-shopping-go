//go:build integration

package testutil

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/pad01g/proxy-shopping-go/node/internal/bitcoinrpc"
)

// BitcoindImage is the image of the lab bitcoind.
const BitcoindImage = "bitcoin/bitcoin:29.0"

// docker talks to the Docker Engine API over its unix socket (the test container has no docker CLI).
type docker struct{ hc *http.Client }

func newDocker() *docker {
	sock := os.Getenv("DOCKER_SOCK")
	if sock == "" {
		sock = "/var/run/docker.sock"
	}
	return &docker{hc: &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}},
	}}
}

func (d *docker) do(method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, "http://docker"+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := d.hc.Do(req)
	if err != nil {
		return fmt.Errorf("docker %s %s: %w", method, path, err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("docker %s %s: HTTP %d: %s", method, path, res.StatusCode, bytes.TrimSpace(data))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// StartBitcoind starts a signet bitcoind with the OP_TRUE challenge (as in the lab) in a container on the
// default bridge network and returns its RPC URL (http://lab:lab@<ip>:38332). The container is removed when
// the test ends. PS_BITCOIND_RPC, if set, is returned instead (an already running node).
func StartBitcoind(t testing.TB) string {
	t.Helper()
	if u := os.Getenv("PS_BITCOIND_RPC"); u != "" {
		return u
	}
	d := newDocker()
	var created struct{ ID string }
	err := d.do(http.MethodPost, "/containers/create", map[string]any{
		"Image": BitcoindImage,
		"Cmd": []string{"-signet", "-signetchallenge=51", "-server", "-txindex", "-rpcuser=lab", "-rpcpassword=lab",
			"-rpcbind=0.0.0.0", "-rpcallowip=0.0.0.0/0", "-fallbackfee=0.0002", "-listen=0", "-dnsseed=0"},
		"HostConfig": map[string]any{"AutoRemove": true},
	}, &created)
	if err != nil {
		t.Skipf("cannot create a bitcoind container (docker socket needed): %v", err)
	}
	t.Cleanup(func() { _ = d.do(http.MethodDelete, "/containers/"+created.ID+"?force=true", nil, nil) })
	if err := d.do(http.MethodPost, "/containers/"+created.ID+"/start", nil, nil); err != nil {
		t.Fatal(err)
	}
	var info struct {
		NetworkSettings struct {
			IPAddress string
			Networks  map[string]struct{ IPAddress string }
		}
	}
	if err := d.do(http.MethodGet, "/containers/"+created.ID+"/json", nil, &info); err != nil {
		t.Fatal(err)
	}
	ip := info.NetworkSettings.IPAddress
	for _, n := range info.NetworkSettings.Networks {
		if ip == "" {
			ip = n.IPAddress
		}
	}
	if ip == "" {
		t.Fatal("bitcoind container has no IP address")
	}
	url := fmt.Sprintf("http://lab:lab@%s:38332", ip)
	rpc, err := bitcoinrpc.New(url)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, err := rpc.GetBlockCount(ctx)
		cancel()
		if err == nil {
			return url
		}
		if time.Now().After(deadline) {
			t.Fatalf("bitcoind did not answer: %v", err)
		}
		time.Sleep(300 * time.Millisecond)
	}
}
