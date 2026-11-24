// Package cluster is the root of skep-cluster's packages (ADR 0013): the
// unprivileged service that holds the node key, the cluster membership and
// the replicated store. Every host is a one-node cluster (step 8a) until it
// joins another (step 8b-1: join-request, admit, join; Raft over TLS 1.3
// with per-RPC identity in package raftnet).
//
// The Raft library is used only by the store and raftnet packages;
// everything else sees the store through small interfaces
// (localapi.Backend, service.Store, raftnet.Trust).
package cluster
