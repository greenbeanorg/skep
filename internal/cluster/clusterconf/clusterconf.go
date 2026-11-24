// Package clusterconf is skep-cluster's configuration file, cluster.json
// (ADR 0013 B1). The root-run commands "skep-cluster init" and
// "skep-cluster listen" write it; the service reads it and cannot change
// it (root:skep-cluster, 0640).
package clusterconf

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
)

// Defaults of a packaged install.
const (
	// DefaultPath is the configuration file.
	DefaultPath = "/usr/local/etc/skep/cluster.json"
	// DefaultDataDir holds the node key and the Raft store. It belongs
	// to the service account, mode 0700.
	DefaultDataDir = "/var/db/skep/cluster"
	// DefaultSocket is the service's Unix socket. Its directory belongs
	// to the service account with the group skep, mode 0750, so only
	// the service, skep-api and root can reach the socket.
	DefaultSocket = "/var/run/skep-cluster/cluster.sock"
	// DefaultAgentSocket is skep-agent's socket. Only root-run commands
	// use it; the service never does (ADR 0013 A3).
	DefaultAgentSocket = "/var/run/skep/agent.sock"
	// Account is the service account (UID and GID 410).
	Account = "skep-cluster"
	// APIAccount is the account skep-api runs as.
	APIAccount = "skep"
	// Port is the TCP port of the node-to-node listener. It is open on
	// clustered nodes only.
	Port = 8444
	// FileMode is the mode of cluster.json.
	FileMode fs.FileMode = 0o640
)

// Config is the content of cluster.json.
type Config struct {
	// NodeID is the node's ID: the UUID skep-agent created in agent.db,
	// read by "skep-cluster init".
	NodeID string `json:"node_id"`
	// NodeName is the node's name in the membership.
	NodeName string `json:"node_name"`
	// ListenAddress is the address the node-to-node listener binds, an
	// IP address without a port. Empty means no network listener: a
	// single host.
	ListenAddress string `json:"listen_address,omitempty"`
}

var (
	uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	nameRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
)

// ValidNodeID reports whether id has the form of a node ID.
func ValidNodeID(id string) bool { return uuidRE.MatchString(id) }

// ParseListenAddress checks a listen address: one IP address, no port, no
// zone, and not the unspecified address (the listener binds one address
// only; there is no default).
func ParseListenAddress(s string) (netip.Addr, error) {
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("listen address %q is not an IP address (give it without a port)", s)
	}
	if a.Zone() != "" {
		return netip.Addr{}, fmt.Errorf("listen address %q must not carry a zone", s)
	}
	if a.IsUnspecified() {
		return netip.Addr{}, fmt.Errorf("listen address %q is the unspecified address; name the one address to listen on", s)
	}
	if a.IsMulticast() {
		return netip.Addr{}, fmt.Errorf("listen address %q is a multicast address", s)
	}
	return a.Unmap(), nil
}

// Validate checks every field.
func (c Config) Validate() error {
	if !ValidNodeID(c.NodeID) {
		return fmt.Errorf("node_id %q is not a node ID (a lower-case UUID)", c.NodeID)
	}
	if !nameRE.MatchString(c.NodeName) {
		return fmt.Errorf("node_name %q is not valid: use letters, digits, dots and hyphens", c.NodeName)
	}
	if c.ListenAddress != "" {
		a, err := ParseListenAddress(c.ListenAddress)
		if err != nil {
			return err
		}
		if a.String() != c.ListenAddress {
			return fmt.Errorf("listen_address %q must be written as %q", c.ListenAddress, a.String())
		}
	}
	return nil
}

// PeerAddress is the address other nodes reach this node on, or "" for a
// node without a listener.
func (c Config) PeerAddress() string {
	if c.ListenAddress == "" {
		return ""
	}
	a, err := netip.ParseAddr(c.ListenAddress)
	if err != nil {
		return ""
	}
	return netip.AddrPortFrom(a, Port).String()
}

// ErrNotInitialised is returned by Load when cluster.json does not exist.
var ErrNotInitialised = errors.New("this host has no cluster.json: run \"skep-cluster init\" as root first")

// Load reads and validates cluster.json. Unknown fields are refused.
func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Config{}, fmt.Errorf("%s: %w", path, ErrNotInitialised)
	}
	if err != nil {
		return Config{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if dec.More() {
		return Config{}, fmt.Errorf("%s: more than one JSON value", path)
	}
	if err := c.Validate(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Write replaces cluster.json atomically, with mode 0640. uid and gid are
// its owner and group; -1 leaves either as the process's own (tests and
// development). Its directory must exist.
func Write(path string, c Config, uid, gid int) error {
	if err := c.Validate(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := path + ".tmp-" + strconv.Itoa(os.Getpid())
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if _, err := f.Write(b); err != nil {
		return fail(err)
	}
	if uid >= 0 || gid >= 0 {
		if err := f.Chown(uid, gid); err != nil {
			return fail(err)
		}
	}
	if err := f.Chmod(FileMode); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}
