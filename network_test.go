package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	kolodht "github.com/ciranda-dev/kolo-dht"
	koloswarm "github.com/ciranda-dev/kolo-swarm"
)

// publicTouched counts the starts that would have bootstrapped from the public network.
var publicTouched atomic.Int32

var errPublicInTest = errors.New("a test tried to bootstrap from the public network: pass -bootstrap to a loopback node")

// TestMain keeps every test off the public network: a start without -bootstrap fails instead of sending a packet.
func TestMain(m *testing.M) {
	real := bootstrapSwarm
	bootstrapSwarm = func(ctx context.Context, sw *koloswarm.Swarm, set settings) error {
		if len(set.bootstrap) == 0 {
			publicTouched.Add(1)
			return errPublicInTest
		}
		return real(ctx, sw, set)
	}
	code := m.Run()
	if n := publicTouched.Load(); n != 0 && code == 0 {
		fmt.Fprintf(os.Stderr, "%d starts used the public network\n", n)
		code = 1
	}
	os.Exit(code)
}

// withBootstrap replaces what a start calls to join the network, until the test ends.
func withBootstrap(t *testing.T, f func(context.Context, *koloswarm.Swarm, settings) error) {
	t.Helper()
	old := bootstrapSwarm
	bootstrapSwarm = f
	t.Cleanup(func() { bootstrapSwarm = old })
}

func resolveArgs(t *testing.T, args []string, env map[string]string) (settings, error) {
	t.Helper()
	cfg, err := LoadConfig(args, func(k string) string { return env[k] }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = dataDir(t)
	return cfg.resolve()
}

// The default is the public network: no entry node is set, so kolo-swarm uses the list built into kolo-dht, and the
// app listens beyond loopback to reach it.
func TestDefaultIsThePublicNetwork(t *testing.T) {
	set, err := resolveArgs(t, nil, nil)
	if err != nil || len(set.bootstrap) != 0 {
		t.Fatalf("default: %v, %v", set.bootstrap, err)
	}
	if set.Listen != publicListen {
		t.Errorf("Listen = %q, want %q", set.Listen, publicListen)
	}
	net := set.network()
	var want []string
	for _, n := range kolodht.PublicServers() {
		want = append(want, n.String())
	}
	if net.Mode != "public" || len(want) == 0 || !slices.Equal(net.Nodes, want) {
		t.Errorf("network = %+v, want the public list %v", net, want)
	}
}

// -bootstrap alone is your own network, used alone: the public list is not in the settings.
func TestBootstrapIsYourOwnNetwork(t *testing.T) {
	set, err := resolveArgs(t, []string{"-bootstrap", "127.0.0.1:4000, 127.0.0.1:4001,127.0.0.1:4000"}, nil)
	if err != nil || !slices.Equal(set.bootstrap, []string{"127.0.0.1:4000", "127.0.0.1:4001"}) {
		t.Fatalf("own: %v, %v", set.bootstrap, err)
	}
	if net := set.network(); net.Mode != "own" || !slices.Equal(net.Nodes, set.bootstrap) {
		t.Errorf("network = %+v", net)
	}
	if set.Listen != loopbackListen {
		t.Errorf("Listen = %q: every entry node is on loopback, want %q", set.Listen, loopbackListen)
	}

	set, err = resolveArgs(t, nil, map[string]string{"KOLO_CHAT_BOOTSTRAP": "192.168.1.5:7447"})
	if err != nil || !slices.Equal(set.bootstrap, []string{"192.168.1.5:7447"}) || set.Listen != publicListen {
		t.Errorf("env: %v, %q, %v", set.bootstrap, set.Listen, err)
	}
	set, err = resolveArgs(t, []string{"-bootstrap", "127.0.0.1:4000", "-listen", "10.0.0.2:0"}, nil)
	if err != nil || set.Listen != "10.0.0.2:0" {
		t.Errorf("a given -listen wins: %q, %v", set.Listen, err)
	}
}

func TestBadBootstrapIsRefused(t *testing.T) {
	for _, v := range []string{"x", "127.0.0.1", "127.0.0.1:0", "[::1]:4000", "[::ffff:127.0.0.1]:4000", ",", " "} {
		_, err := (Config{DataDir: dataDir(t), Bootstrap: v}).resolve()
		var ce *ConfigError
		if !errors.Is(err, ErrMalformed) || !errors.Is(err, ErrConfig) || !errors.As(err, &ce) || ce.Setting != "bootstrap" {
			t.Errorf("%q: got %v", v, err)
		}
	}
	var many []string
	for i := range 17 {
		many = append(many, fmt.Sprintf("10.0.0.%d:7447", i+1))
	}
	if _, err := (Config{DataDir: dataDir(t), Bootstrap: strings.Join(many, ",")}).resolve(); !errors.Is(err, ErrMalformed) {
		t.Errorf("17 nodes: got %v", err)
	}
}

// A start without -bootstrap reaches kolo-swarm with an empty BootstrapNodes, which is the public network; with
// -bootstrap the swarm gets the list alone.
func TestStartPassesTheNetworkToTheSwarm(t *testing.T) {
	var got []string
	var called bool
	withBootstrap(t, func(_ context.Context, sw *koloswarm.Swarm, set settings) error {
		called, got = true, set.bootstrap
		return errors.New("stop here")
	})
	c := NewChatService(Config{DataDir: dataDir(t)}, func(string, any) {})
	if _, err := c.Start(); err == nil || !called || len(got) != 0 {
		t.Fatalf("default: called %v, nodes %v, %v", called, got, err)
	}
	c = NewChatService(Config{DataDir: dataDir(t), Bootstrap: "127.0.0.1:4000"}, func(string, any) {})
	if _, err := c.Start(); err == nil || !slices.Equal(got, []string{"127.0.0.1:4000"}) {
		t.Fatalf("own: nodes %v, %v", got, err)
	}
}

// When no entry node answers, the error names the three ways to connect, and the app does not try another network.
func TestUnreachableNetworkNamesTheOptions(t *testing.T) {
	for _, tc := range []struct {
		name, bootstrap, mode string
	}{
		{"public", "", "public"},
		{"own", "127.0.0.1:9", "own"},
	} {
		calls := 0
		withBootstrap(t, func(context.Context, *koloswarm.Swarm, settings) error {
			calls++
			return fmt.Errorf("koloswarm: %w", kolodht.ErrNoNodes)
		})
		c := NewChatService(Config{DataDir: dataDir(t), Bootstrap: tc.bootstrap}, func(string, any) {})
		_, err := c.Start()
		var ne *NetworkError
		if !errors.Is(err, ErrNetwork) || !errors.Is(err, kolodht.ErrNoNodes) || !errors.As(err, &ne) || ne.Network.Mode != tc.mode || calls != 1 {
			t.Fatalf("%s: %v (calls %d)", tc.name, err, calls)
		}
		msg := err.Error()
		for _, want := range []string{"ublic network", "-bootstrap host:port", "-first", "-bootstrap 127.0.0.1:PORT", "Ciranda", "no uptime guarantee"} {
			if !strings.Contains(msg, want) {
				t.Errorf("%s: %q lacks %q", tc.name, msg, want)
			}
		}
		if tc.mode == "own" && !strings.Contains(msg, "127.0.0.1:9") {
			t.Errorf("%s: %q does not name the entry nodes", tc.name, msg)
		}
	}
}

// Another failure is not turned into a network error.
func TestOtherBootstrapErrorsPassThrough(t *testing.T) {
	other := errors.New("boom")
	withBootstrap(t, func(context.Context, *koloswarm.Swarm, settings) error { return other })
	c := NewChatService(Config{DataDir: dataDir(t)}, func(string, any) {})
	if _, err := c.Start(); !errors.Is(err, other) || errors.Is(err, ErrNetwork) {
		t.Fatalf("got %v", err)
	}
}

// A conversation that did not start for want of a network says so in its row, without a folder to move aside, and
// Start reports which network the app joins.
func TestUnreachableNetworkInTheList(t *testing.T) {
	withBootstrap(t, func(context.Context, *koloswarm.Swarm, settings) error { return kolodht.ErrNoNodes })
	n := newNetwork(t)
	cfg := n.app(dataDir(t))
	app := NewConversations(cfg, (&convEvents{}).emit)
	info, err := app.Start()
	if err != nil || info.Network.Mode != "own" {
		t.Fatalf("Start: %v (%+v)", err, info)
	}
	if _, err := app.Create("x"); !errors.Is(err, ErrNetwork) {
		t.Fatalf("Create: %v", err)
	}
	err = &NetworkError{Network: NetworkInfo{Mode: "public"}, Err: kolodht.ErrNoNodes}
	row := convEntry{id: "a", dir: "/x", err: err}.info()
	if row.Network != "public" || row.Folder != "" || row.Error == "" {
		t.Errorf("row = %+v", row)
	}
}

// A conversation that was fine before is listed with the network error when the entry nodes do not answer at the
// next start, and starts again once they do.
func TestRestartOnAnUnreachableNetwork(t *testing.T) {
	n := newNetwork(t)
	cfg := n.app(dataDir(t))
	a, _ := startApp(t, cfg)
	mustCreateConv(t, a, "Family")
	a.ServiceShutdown()

	down := cfg
	down.Bootstrap = "127.0.0.1:9"
	b := NewConversations(down, (&convEvents{}).emit)
	t.Cleanup(func() { b.ServiceShutdown() })
	info, err := b.Start()
	if err != nil || len(info.Conversations) != 1 {
		t.Fatalf("Start: %v (%+v)", err, info)
	}
	row := info.Conversations[0]
	if row.Network != "own" || row.Folder != "" || !strings.Contains(row.Error, "could not reach your network") {
		t.Errorf("row = %+v", row)
	}

	c, _ := startApp(t, cfg)
	if rows := c.List(); len(rows) != 1 || rows[0].Error != "" {
		t.Errorf("after the entry node is back: %+v", rows)
	}
}
