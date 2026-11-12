package main

import (
	"bufio"
	"context"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/greenbeanorg/skep/internal/ctlstate"
	"github.com/greenbeanorg/skep/internal/termpass"
	"github.com/greenbeanorg/skep/pkg/client"
)

// maxPasswordBytes bounds what is read as a password: far above any
// password the server accepts.
const maxPasswordBytes = termpass.MaxLength

// resolveServer returns the normalized server: the one given, or the
// server of the last login.
func resolveServer(store *ctlstate.Store, given string) (string, error) {
	if given != "" {
		return client.NormalizeServer(given)
	}
	cur, err := store.Current()
	if err != nil {
		return "", err
	}
	if cur == "" {
		return "", errors.New("no server given and no previous login: use --server HOST[:PORT]")
	}
	return cur, nil
}

// session is a connection to a server with a stored session.
type session struct {
	store  *ctlstate.Store
	server string
	client *client.Client
}

// clientOptions turns a stored trust setting into client options. The CA
// file is read on every run, so replacing it takes effect at once.
func clientOptions(server string, t ctlstate.Trust, token string) (client.Options, error) {
	opts := client.Options{Server: server, Pin: t.PublicKeySHA256, Token: token, UserAgent: userAgent()}
	if t.VerifiesChain() {
		opts.VerifyChain = true
		if t.CAFile != "" {
			pool, err := client.LoadCAFile(t.CAFile)
			if err != nil {
				return client.Options{}, fmt.Errorf("the CA file for %s: %w", server, err)
			}
			opts.RootCAs = pool
		}
	}
	return opts, nil
}

// newClient returns a client for a server with its stored trust setting.
func newClient(server string, t ctlstate.Trust, token string) (*client.Client, error) {
	opts, err := clientOptions(server, t, token)
	if err != nil {
		return nil, err
	}
	return client.New(opts)
}

// connect opens the stored session for a server. It needs both a stored
// trust setting and a session token; it never trusts a server by itself.
func connect(e env, serverFlag string) (*session, error) {
	store := ctlstate.New(e.stateDir)
	server, err := resolveServer(store, serverFlag)
	if err != nil {
		return nil, err
	}
	trust, ok, err := store.Trust(server)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("no trust setting for %s: run skepctl login --server %s", server, server)
	}
	sess, ok, err := store.Session(server)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("not logged in to %s: run skepctl login --server %s", server, server)
	}
	c, err := newClient(server, trust, sess.Token)
	if err != nil {
		return nil, err
	}
	return &session{store: store, server: server, client: c}, nil
}

// explain turns the errors every command can meet into instructions. An
// ended session's token is forgotten, since it can never work again.
func (s *session) explain(err error) error {
	var km *client.KeyMismatchError
	switch {
	case errors.As(err, &km):
		return keyChangedError(s.server, km)
	case client.IsUntrustedCertificate(err):
		return untrustedError(s.server, err)
	case client.IsUnauthenticated(err):
		if derr := s.store.DeleteSession(s.server); derr != nil {
			return fmt.Errorf("the session with %s has ended, and its token could not be forgotten: %w", s.server, derr)
		}
		return fmt.Errorf("the session with %s has ended (expired, logged out, or revoked): run skepctl login", s.server)
	}
	return err
}

// keyChangedError explains a server that presents another key than the
// pinned one. Nothing was sent to it.
func keyChangedError(server string, km *client.KeyMismatchError) error {
	return fmt.Errorf(`the public key of %s has changed; nothing was sent to it.
  pinned:    %s
  presented: %s
This happens after "skep-api tls regenerate" on the host, and it is also what
an attack in the middle looks like. Run "skep-api tls fingerprint" on the host.
Only if it prints the presented key, accept it with:
  skepctl login --server %s --username NAME --trust-key %s
If the server now has an operator-supplied certificate, use --ca-file FILE or
--system-roots instead of --trust-key.`,
		server, km.Pinned, km.Presented, server, km.Presented)
}

// untrustedError explains a certificate that failed chain or host name
// verification. Nothing was sent to the server.
func untrustedError(server string, err error) error {
	return fmt.Errorf("the certificate of %s is not trusted; nothing was sent to it: %w\n"+
		"If the operator's CA has changed, log in again with --ca-file NEWFILE (or --system-roots).", server, err)
}

// trustFlags are the login flags that decide how a server is trusted.
type trustFlags struct {
	// key is --trust-key: the pin.
	key string
	// caFile is --ca-file; systemRoots is --system-roots. Either one
	// selects chain verification.
	caFile      string
	systemRoots bool
}

func (f trustFlags) chain() bool { return f.caFile != "" || f.systemRoots }

// establishTrust makes sure the server has a stored trust setting that the
// user chose, before any password is asked for:
//
//   - --ca-file or --system-roots: chain and host name verification, with
//     --trust-key as an optional extra pin. Checked with a handshake
//     before it is stored.
//   - an entry in chain mode and no flags: kept as it is.
//   - otherwise a pinned key, confirmed on first contact (trustServer).
func establishTrust(ctx context.Context, e env, store *ctlstate.Store, server string, f trustFlags) error {
	stored, have, err := store.Trust(server)
	if err != nil {
		return err
	}
	if f.chain() {
		t := ctlstate.Trust{Mode: ctlstate.TrustCA, PublicKeySHA256: f.key, TrustedAt: e.now().UTC()}
		if f.caFile != "" {
			if t.CAFile, err = filepath.Abs(f.caFile); err != nil {
				return err
			}
		}
		opts, err := clientOptions(server, t, "")
		if err != nil {
			return err
		}
		if err := client.CheckConnection(ctx, opts); err != nil {
			var km *client.KeyMismatchError
			if errors.As(err, &km) {
				return fmt.Errorf("%s presents public key %s, not %s as given with --trust-key; nothing was trusted",
					server, km.Presented, km.Pinned)
			}
			if client.IsUntrustedCertificate(err) {
				return fmt.Errorf("the certificate of %s does not verify against %s; nothing was trusted: %w",
					server, rootsName(t), err)
			}
			return err
		}
		if have && t.CAFile == stored.CAFile && t.PublicKeySHA256 == stored.PublicKeySHA256 && stored.VerifiesChain() {
			return nil
		}
		if have && !stored.VerifiesChain() {
			fmt.Fprintf(e.stderr, "Switching %s from the pinned key %s to verifying its certificate against %s.\n",
				server, stored.PublicKeySHA256, rootsName(t))
		} else {
			fmt.Fprintf(e.stderr, "The certificate of %s verifies against %s.\n", server, rootsName(t))
		}
		return store.SetTrust(server, t)
	}
	if have && stored.VerifiesChain() {
		if f.key == "" {
			return nil
		}
		// --trust-key alone switches back to a pinned key.
		fmt.Fprintf(e.stderr, "Switching %s from certificate verification to a pinned key.\n", server)
		return trustServer(ctx, e, store, server, f.key, false)
	}
	return trustServer(ctx, e, store, server, f.key, have)
}

// rootsName names what a chain is verified against, for messages.
func rootsName(t ctlstate.Trust) string {
	if t.CAFile != "" {
		return t.CAFile
	}
	return "the system roots"
}

// describeCertificate shows what the user is asked to trust.
func describeCertificate(w io.Writer, server string, cert *x509.Certificate) {
	names := append([]string(nil), cert.DNSNames...)
	for _, ip := range cert.IPAddresses {
		names = append(names, ip.String())
	}
	fmt.Fprintf(w, "The server %s presented this certificate:\n", server)
	fmt.Fprintf(w, "  public key:   %s\n", client.PublicKeyPin(cert))
	fmt.Fprintf(w, "  subject:      %s\n", cert.Subject.String())
	if len(names) > 0 {
		fmt.Fprintf(w, "  names:        %s\n", strings.Join(names, ", "))
	}
	fmt.Fprintf(w, "  valid until:  %s\n", cert.NotAfter.UTC().Format(time.RFC3339))
}

// trustServer makes sure a pin is stored for the server that the user has
// confirmed, either on the terminal or by naming it with --trust-key.
// A pinned key is replaced only by naming the new one. havePin says
// whether a pin is stored for the server now.
func trustServer(ctx context.Context, e env, store *ctlstate.Store, server, trustKey string, havePin bool) error {
	stored, _, err := store.Trust(server)
	if err != nil {
		return err
	}
	have := havePin
	if have && trustKey == stored.PublicKeySHA256 {
		return nil
	}

	// Look at the key first, so that a changed key is reported before
	// the password is asked for. The login itself checks it again.
	cert, err := client.FetchCertificate(ctx, server)
	if err != nil {
		return err
	}
	presented := client.PublicKeyPin(cert)

	switch {
	case have && trustKey == "":
		if presented != stored.PublicKeySHA256 {
			return keyChangedError(server, &client.KeyMismatchError{Pinned: stored.PublicKeySHA256, Presented: presented})
		}
		return nil
	case trustKey != "":
		if presented != trustKey {
			describeCertificate(e.stderr, server, cert)
			return fmt.Errorf("%s presents public key %s, not %s as given with --trust-key; nothing was trusted",
				server, presented, trustKey)
		}
		if have {
			fmt.Fprintf(e.stderr, "Replacing the pinned key of %s:\n  old: %s\n  new: %s\n", server, stored.PublicKeySHA256, presented)
		}
	default: // first contact, no --trust-key
		describeCertificate(e.stderr, server, cert)
		fmt.Fprintf(e.stderr, "skepctl has not connected to this server before. Compare the public key with the\n"+
			"output of \"skep-api tls fingerprint\" on the host before you trust it.\n")
		ok, err := e.confirm(ctx, "Trust this key? [y/N] ")
		if err != nil {
			if errors.Is(err, termpass.ErrNoTerminal) {
				return fmt.Errorf("there is no terminal to confirm the key on; pass it with --trust-key %s once you have "+
					"compared it (for an operator-supplied certificate, use --ca-file FILE or --system-roots)", presented)
			}
			return err
		}
		if !ok {
			return errors.New("the key was not trusted; nothing was sent to the server")
		}
	}
	return store.SetTrust(server, ctlstate.Trust{Mode: ctlstate.TrustPin, PublicKeySHA256: presented, TrustedAt: e.now().UTC()})
}

func readPassword(ctx context.Context, e env, fromStdin bool, prompt string) (string, error) {
	if fromStdin {
		line, err := bufio.NewReaderSize(io.LimitReader(e.stdin, maxPasswordBytes), maxPasswordBytes).ReadString('\n')
		if err != nil && line == "" {
			return "", errors.New("no password on standard input")
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	pw, err := e.readPassword(ctx, prompt)
	if errors.Is(err, termpass.ErrNoTerminal) {
		return "", fmt.Errorf("%w; run skepctl on a terminal, or pass --password-stdin", err)
	}
	return pw, err
}

func cmdLogin(ctx context.Context, args []string, e env) error {
	fs := flag.NewFlagSet("skepctl login", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	serverFlag := fs.String("server", "", "address of skep-api, HOST[:PORT] (default port "+client.DefaultPort+")")
	username := fs.String("username", "", "user to log in as")
	fromStdin := fs.Bool("password-stdin", false, "read the password from standard input instead of the terminal")
	trustKey := fs.String("trust-key", "", `public key pin to trust, as printed by "skep-api tls fingerprint"; `+
		"needed to accept a changed key, and instead of the question on first contact; with --ca-file or "+
		"--system-roots, an optional extra check")
	caFile := fs.String("ca-file", "", "for an operator-supplied certificate: verify the chain and host name against "+
		"the CA certificates in this PEM file instead of pinning the key")
	systemRoots := fs.Bool("system-roots", false, "for an operator-supplied certificate: verify the chain and host name "+
		"against the system's CA certificates instead of pinning the key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *username == "" {
		return errors.New("--username is required")
	}
	if *trustKey != "" && !client.ValidPin(*trustKey) {
		return fmt.Errorf("--trust-key must be sha256: followed by 64 lowercase hex digits, as \"skep-api tls fingerprint\" prints it")
	}
	if *caFile != "" && *systemRoots {
		return errors.New("--ca-file and --system-roots cannot be used together")
	}
	store := ctlstate.New(e.stateDir)
	server, err := resolveServer(store, *serverFlag)
	if err != nil {
		return err
	}
	if err := establishTrust(ctx, e, store, server, trustFlags{key: *trustKey, caFile: *caFile, systemRoots: *systemRoots}); err != nil {
		return err
	}
	trust, _, err := store.Trust(server)
	if err != nil {
		return err
	}

	password, err := readPassword(ctx, e, *fromStdin, fmt.Sprintf("Password for %s at %s: ", *username, server))
	if err != nil {
		return err
	}
	anon, err := newClient(server, trust, "")
	if err != nil {
		return err
	}
	defer anon.Close()
	res, err := anon.Login(ctx, *username, password)
	if err != nil {
		var km *client.KeyMismatchError
		if errors.As(err, &km) {
			return keyChangedError(server, km)
		}
		if client.IsUntrustedCertificate(err) {
			return untrustedError(server, err)
		}
		return fmt.Errorf("login failed: %w", err)
	}

	// The previous session in skepctl's own token file is ended, not just
	// forgotten. This is a logout with that one token: the server ends
	// only the session the token belongs to, never the user's other
	// sessions (a browser, another skepctl).
	if old, ok, err := store.Session(server); err == nil && ok && old.Token != res.Token {
		if c, err := newClient(server, trust, old.Token); err == nil {
			_ = c.Logout(ctx) // best effort: it may have expired already
			c.Close()
		}
	}
	if err := store.SetSession(server, ctlstate.Session{
		Username: res.User.Username, Token: res.Token, CreatedAt: res.Session.CreatedAt, ExpiresAt: res.Session.ExpiresAt,
	}); err != nil {
		// Do not leave a live session behind that nobody holds.
		c, cerr := newClient(server, trust, res.Token)
		if cerr == nil {
			_ = c.Logout(ctx)
			c.Close()
		}
		return fmt.Errorf("logged in, but the session could not be stored, so it was ended again: %w", err)
	}
	idle := time.Duration(res.Session.IdleTimeoutSeconds) * time.Second
	fmt.Fprintf(e.stdout, "Logged in to %s as %s (role %s).\n", server, res.User.Username, res.User.Role)
	fmt.Fprintf(e.stdout, "The session ends at %s, or after %s without use.\n",
		res.Session.ExpiresAt.Local().Format(time.RFC3339), idle)
	return nil
}

func cmdLogout(ctx context.Context, args []string, e env) error {
	fs := flag.NewFlagSet("skepctl logout", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	serverFlag := fs.String("server", "", "address of skep-api (default: the server of the last login)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	s, err := connect(e, *serverFlag)
	if err != nil {
		return err
	}
	defer s.client.Close()
	err = s.client.Logout(ctx)
	switch {
	case err == nil:
	case client.IsUnauthenticated(err):
		// Already ended on the server; only the local copy is left.
	default:
		var km *client.KeyMismatchError
		if errors.As(err, &km) {
			return keyChangedError(s.server, km)
		}
		return fmt.Errorf("the session was not ended on the server, and its token is kept so you can try again: %w", err)
	}
	if err := s.store.DeleteSession(s.server); err != nil {
		return fmt.Errorf("the session was ended on the server, but its token could not be removed: %w", err)
	}
	fmt.Fprintf(e.stdout, "Logged out of %s.\n", s.server)
	return nil
}
