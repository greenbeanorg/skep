// Package cluster is the root of skep-cluster's packages (ADR 0013): the
// unprivileged service that holds the node key, the cluster membership and
// the replicated store. Step 8a builds a one-node cluster: no join, no
// second node, no inbound Raft RPC.
//
// The Raft library is used only by the store package; everything else sees
// the store through small interfaces (localapi.Backend, service.Store).
package cluster
