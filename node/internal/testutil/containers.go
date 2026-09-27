//go:build integration

package testutil

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// AnvilImage is the image of the lab anvil.
const AnvilImage = "ghcr.io/foundry-rs/foundry:stable"

// StartContainer runs an image on the default bridge network and returns its IP; it is removed when the test ends.
func StartContainer(t testing.TB, image string, entrypoint, cmd []string) string {
	t.Helper()
	d := newDocker()
	spec := map[string]any{"Image": image, "Cmd": cmd, "HostConfig": map[string]any{"AutoRemove": true}}
	if entrypoint != nil {
		spec["Entrypoint"] = entrypoint
	}
	var created struct{ ID string }
	if err := d.do(http.MethodPost, "/containers/create", spec, &created); err != nil {
		t.Skipf("cannot create a %s container (docker socket needed): %v", image, err)
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
		t.Fatalf("%s container has no IP address", image)
	}
	return ip
}

// StartAnvil runs anvil (chain id 31337) and returns its RPC URL once it answers.
func StartAnvil(t testing.TB) string {
	t.Helper()
	ip := StartContainer(t, AnvilImage, []string{"anvil"}, []string{"--host", "0.0.0.0", "--chain-id", "31337"})
	url := fmt.Sprintf("http://%s:8545", ip)
	deadline := time.Now().Add(60 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`))
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		cancel()
		if err == nil {
			res.Body.Close()
			return url
		}
		if time.Now().After(deadline) {
			t.Fatalf("anvil did not answer: %v", err)
		}
		time.Sleep(300 * time.Millisecond)
	}
}
