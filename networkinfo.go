package main

import (
	"slices"

	kolodht "github.com/ciranda-dev/kolo-dht"
)

// NetworkInfo says which network the app joins: Mode is "public" (the entry nodes built into Kolo, run by Ciranda)
// or "own" (the -bootstrap list), and Nodes are the entry nodes in use.
type NetworkInfo struct {
	Mode  string   `json:"mode"`
	Nodes []string `json:"nodes"`
}

// network describes the network the settings choose.
func (s settings) network() NetworkInfo {
	if len(s.bootstrap) > 0 {
		return NetworkInfo{Mode: "own", Nodes: slices.Clone(s.bootstrap)}
	}
	var nodes []string
	for _, n := range kolodht.PublicServers() {
		nodes = append(nodes, n.String())
	}
	return NetworkInfo{Mode: "public", Nodes: nodes}
}
