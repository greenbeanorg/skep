// Package ctlstate keeps skepctl's local state: the public key pinned for
// each server, and the bearer session token for each server.
//
// Both live in one directory, by default $XDG_CONFIG_HOME/skepctl (on
// FreeBSD and Linux usually ~/.config/skepctl), created 0700. Each file
// is written 0600 through a temporary file and a rename, so a reader never
// sees half a file. A file or directory that other users could read or
// change is refused instead of used: a readable session file leaks the
// token, and a writable pin file would let someone else replace the key
// skepctl trusts (ADR 0006 decisions 3 and 6).
package ctlstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// File names inside the state directory.
const (
	PinsFile     = "pins.json"
	SessionsFile = "sessions.json"
)

// formatVersion is the version of both files' format.
const formatVersion = 1

// DirEnv names the environment variable that overrides the directory.
const DirEnv = "SKEPCTL_CONFIG_DIR"

// DefaultDir returns the state directory: $SKEPCTL_CONFIG_DIR if set,
// otherwise "skepctl" in the user's configuration directory.
func DefaultDir() (string, error) {
	if d := os.Getenv(DirEnv); d != "" {
		return d, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("cannot find a configuration directory for skepctl: %w", err)
	}
	return filepath.Join(base, "skepctl"), nil
}

// Pin is a server's trusted public key.
type Pin struct {
	// PublicKeySHA256 is the pin, as "sha256:<hex>".
	PublicKeySHA256 string    `json:"public_key_sha256"`
	TrustedAt       time.Time `json:"trusted_at"`
}

// Session is a bearer session with a server.
type Session struct {
	Username string `json:"username"`
	// Token is the bearer token. It is the secret in this package.
	Token     string    `json:"token"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type pinsFile struct {
	Version int            `json:"version"`
	Servers map[string]Pin `json:"servers"`
}

type sessionsFile struct {
	Version int `json:"version"`
	// Current is the server commands use when none is given.
	Current  string             `json:"current,omitempty"`
	Sessions map[string]Session `json:"sessions"`
}

// Store reads and writes the state directory. It does not lock: two
// skepctl processes changing state at the same moment can lose one
// change, never corrupt a file.
type Store struct {
	dir string
	uid int
}

// New returns the store for a directory. Nothing is created until
// something is written.
func New(dir string) *Store { return &Store{dir: dir, uid: os.Geteuid()} }

// Dir returns the state directory.
func (s *Store) Dir() string { return s.dir }

// Pin returns the pin stored for a server.
func (s *Store) Pin(server string) (Pin, bool, error) {
	var f pinsFile
	if err := s.read(PinsFile, &f); err != nil {
		return Pin{}, false, err
	}
	p, ok := f.Servers[server]
	return p, ok, nil
}

// SetPin stores the pin for a server, replacing any other.
func (s *Store) SetPin(server string, p Pin) error {
	var f pinsFile
	if err := s.read(PinsFile, &f); err != nil {
		return err
	}
	if f.Servers == nil {
		f.Servers = map[string]Pin{}
	}
	f.Servers[server] = p
	f.Version = formatVersion
	return s.write(PinsFile, f)
}

// Session returns the session stored for a server.
func (s *Store) Session(server string) (Session, bool, error) {
	var f sessionsFile
	if err := s.read(SessionsFile, &f); err != nil {
		return Session{}, false, err
	}
	sess, ok := f.Sessions[server]
	return sess, ok, nil
}

// Current returns the server commands use when none is given: the one
// logged in to last. It is empty when there is none.
func (s *Store) Current() (string, error) {
	var f sessionsFile
	if err := s.read(SessionsFile, &f); err != nil {
		return "", err
	}
	return f.Current, nil
}

// SetSession stores the session for a server and makes it the current
// server.
func (s *Store) SetSession(server string, sess Session) error {
	var f sessionsFile
	if err := s.read(SessionsFile, &f); err != nil {
		return err
	}
	if f.Sessions == nil {
		f.Sessions = map[string]Session{}
	}
	f.Sessions[server] = sess
	f.Current = server
	f.Version = formatVersion
	return s.write(SessionsFile, f)
}

// DeleteSession forgets the session for a server. The current server
// stays the default for the next login.
func (s *Store) DeleteSession(server string) error {
	var f sessionsFile
	if err := s.read(SessionsFile, &f); err != nil {
		return err
	}
	if _, ok := f.Sessions[server]; !ok {
		return nil
	}
	delete(f.Sessions, server)
	f.Version = formatVersion
	return s.write(SessionsFile, f)
}

// checkDir refuses a state directory that is not a real directory owned
// by this user with no access for others.
func (s *Store) checkDir() error {
	info, err := os.Lstat(s.dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", s.dir)
	}
	if err := s.checkOwner(s.dir, info); err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s is accessible by other users (mode %04o); run: chmod 700 %s", s.dir, info.Mode().Perm(), s.dir)
	}
	return nil
}

// read loads a file into v. A missing directory or file is empty state.
func (s *Store) read(name string, v any) error {
	if err := s.checkDir(); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	path := filepath.Join(s.dir, name)
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	if err := s.checkOwner(path, info); err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s is accessible by other users (mode %04o) and is not used; run: chmod 600 %s",
			path, info.Mode().Perm(), path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s is not valid: %w", path, err)
	}
	var ver struct {
		Version int `json:"version"`
	}
	_ = json.Unmarshal(data, &ver)
	if ver.Version != formatVersion {
		return fmt.Errorf("%s has format version %d; this skepctl understands version %d", path, ver.Version, formatVersion)
	}
	return nil
}

// write replaces a file with v, 0600, atomically.
func (s *Store) write(name string, v any) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	if err := s.checkDir(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, "."+name+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op after the rename
	// CreateTemp creates 0600 already; set it explicitly anyway.
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(s.dir, name))
}
