// Package compat declares which builds of the node, the CLI/SDKs and the
// server work together. Release versions (semver) only say that a newer
// build exists; they cannot order nightlies and dev builds, and they say
// nothing about what broke. Compatibility is an integer generation instead
// (the Kubernetes version-skew idea), compiled into every binary:
//
//   - Gen: the contract this build speaks. Bump it in the commit that
//     changes the node<->server protocol, the public API, or that fixes
//     something every node must have (a security fix).
//   - Required: peers below it still work but are outdated. An outdated
//     node gets no new sandboxes, and after Config.NodeUpdateGrace its
//     running ones move to other nodes (a node evacuation). An outdated
//     CLI or SDK is warned on every response. A critical fix is a bump of
//     Gen and Required together.
//   - Min: peers below it cannot work at all. The server refuses an
//     incompatible node's connection and an incompatible client's requests
//     with 426 Upgrade Required.
//
// Release rule: raise Min only to a generation that was Required in an
// earlier release, so a server upgrade never cuts off a fleet at once
// (nodes get the grace period first). Upgrade order: server, then nodes,
// then CLIs. A unit test keeps Min <= Required <= Gen.
//
// History (why each bump happened):
//
//	node 1: secure layer (guest channel, encrypted disks, QMP and NIC
//	        sockets in the systemd fd store). Older nodes let their owner
//	        read the guest's memory: Required. Nodes built before
//	        generations send none (0), even those that have the secure
//	        layer: they also lack the update machinery, so they are
//	        installed again (the server tells them how). While no node is
//	        current, outdated ones keep serving (server/compat.go), so
//	        upgrading the server first does not stop the pool.
//	api  1: first declared generation.
//	server 1: first declared generation.
package compat

// Gens are what this build speaks; constants.
const (
	NodeGen   = 1
	APIGen    = 1
	ServerGen = 1 // nodes check it against MinServerGen
)

// Thresholds are variables only so tests can move them; change them here,
// never at run time.
var (
	RequiredNodeGen = 1
	MinNodeGen      = 0

	RequiredAPIGen = 0
	MinAPIGen      = 0

	// MinServerGen: a node whose server is older says so in its status.
	MinServerGen = 0
)

// Header carries the sender's generation: nodes on GET /node/v1/connect,
// the CLI and SDKs on every API request, the server on its responses.
const Header = "X-Crowdvm-Gen"

// ClientStatusHeader is set on API responses to an outdated client:
// "outdated; latest=<version>" (latest omitted when the server offers no
// downloads).
const ClientStatusHeader = "X-Crowdvm-Client-Status"

// Node update states, from the server's point of view.
const (
	StateCurrent      = "current"      // up to date
	StateAvailable    = "available"    // a newer release exists; nothing depends on it
	StateOutdated     = "outdated"     // below Required: no new sandboxes, running ones move at the deadline
	StateIncompatible = "incompatible" // below Min: the server refuses the connection
)

// NodeState classifies a node generation.
func NodeState(gen int) string {
	switch {
	case gen < MinNodeGen:
		return StateIncompatible
	case gen < RequiredNodeGen:
		return StateOutdated
	}
	return StateCurrent
}
