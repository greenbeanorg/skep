package main

import (
	"bufio"
	"context"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
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

// connect opens the stored session for a server. It needs both a pinned
// key and a session token; it never trusts a key by itself.
func connect(e env, serverFlag string) (*session, error) {
	store := ctlstate.New(e.stateDir)
	server, err := resolveServer(store, serverFlag)
	if err != nil {
		return nil, err
	}
	pin, ok, err := store.Pin(server)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("no trusted key for %s: run skepctl login --server %s", server, server)
	}
	sess, ok, err := store.Session(server)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("not logged in to %s: run skepctl login --server %s", server, server)
	}
	c, err := client.New(client.Options{Server: server, Pin: pin.PublicKeySHA256, Token: sess.Token, UserAgent: userAgent()})
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
  skepctl login --server %s --username NAME --trust-key %s`,
		server, km.Pinned, km.Presented, server, km.Presented)
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
// A pinned key is replaced only by naming the new one.
func trustServer(ctx context.Context, e env, store *ctlstate.Store, server, trustKey string) error {
	stored, have, err := store.Pin(server)
	if err != nil {
		return err
	}
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
				return fmt.Errorf("%w to confirm the key on; pass it with --trust-key %s once you have compared it", err, presented)
			}
			return err
		}
		if !ok {
			return errors.New("the key was not trusted; nothing was sent to the server")
		}
	}
	return store.SetPin(server, ctlstate.Pin{PublicKeySHA256: presented, TrustedAt: e.now().UTC()})
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
		"needed to accept a changed key, and instead of the question on first contact")
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
	store := ctlstate.New(e.stateDir)
	server, err := resolveServer(store, *serverFlag)
	if err != nil {
		return err
	}
	if err := trustServer(ctx, e, store, server, *trustKey); err != nil {
		return err
	}
	pin, _, err := store.Pin(server)
	if err != nil {
		return err
	}

	password, err := readPassword(ctx, e, *fromStdin, fmt.Sprintf("Password for %s at %s: ", *username, server))
	if err != nil {
		return err
	}
	anon, err := client.New(client.Options{Server: server, Pin: pin.PublicKeySHA256, UserAgent: userAgent()})
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
		return fmt.Errorf("login failed: %w", err)
	}

	// A previous session with this server is ended, not just forgotten.
	if old, ok, err := store.Session(server); err == nil && ok && old.Token != res.Token {
		if c, err := client.New(client.Options{Server: server, Pin: pin.PublicKeySHA256, Token: old.Token, UserAgent: userAgent()}); err == nil {
			_ = c.Logout(ctx) // best effort: it may have expired already
			c.Close()
		}
	}
	if err := store.SetSession(server, ctlstate.Session{
		Username: res.User.Username, Token: res.Token, CreatedAt: res.Session.CreatedAt, ExpiresAt: res.Session.ExpiresAt,
	}); err != nil {
		// Do not leave a live session behind that nobody holds.
		c, cerr := client.New(client.Options{Server: server, Pin: pin.PublicKeySHA256, Token: res.Token, UserAgent: userAgent()})
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
