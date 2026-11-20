package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	kolodht "github.com/ciranda-dev/kolo-dht"
	koloswarm "github.com/ciranda-dev/kolo-swarm"
)

// ErrNetwork matches every NetworkError.
var ErrNetwork = errors.New("the network cannot be reached")

// NetworkError says that no entry node answered, and names the three ways to choose a network.
type NetworkError struct {
	Network NetworkInfo
	Err     error // kolo-dht's ErrNoNodes
}

func (e *NetworkError) Error() string {
	var b strings.Builder
	if e.Network.Mode == "own" {
		fmt.Fprintf(&b, "Kolo Chat could not reach your network: no entry node answered (%s, from -bootstrap). Check that they run and that the addresses are right.", strings.Join(e.Network.Nodes, ", "))
	} else {
		b.WriteString("Kolo Chat could not reach the public network: no entry node answered. Check your internet connection and try again.")
	}
	b.WriteString(" You have three ways to connect. Public network, the default: run by Ciranda, free, with no uptime guarantee while Kolo is a beta.")
	b.WriteString(" Your own network: start Kolo Chat with -bootstrap host:port, the entry node of a kolo-node you run.")
	b.WriteString(" This computer only: run kolo-node with -first on 127.0.0.1 and start Kolo Chat with -bootstrap 127.0.0.1:PORT.")
	return b.String()
}
func (e *NetworkError) Unwrap() error        { return e.Err }
func (e *NetworkError) Is(target error) bool { return target == ErrNetwork }

// bootstrapSwarm is what bootstrap calls: the tests replace it, so that none of them reaches the public network.
var bootstrapSwarm = func(ctx context.Context, sw *koloswarm.Swarm, set settings) error {
	return sw.Bootstrap(ctx, nil) // the swarm reads set.bootstrap from its Config, and uses the public nodes when it is empty
}

// bootstrap joins the network the settings choose, and turns "no entry node answered" into a NetworkError. It never
// falls back to another network.
func bootstrap(ctx context.Context, sw *koloswarm.Swarm, set settings) error {
	err := bootstrapSwarm(ctx, sw, set)
	if errors.Is(err, kolodht.ErrNoNodes) {
		return &NetworkError{Network: set.network(), Err: err}
	}
	return err
}
