package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/ctlstate"
	"github.com/greenbeanorg/skep/pkg/client"
)

// operatorCA issues certificates, as an operator's CA would.
type operatorCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newOperatorCA(t *testing.T) *operatorCA {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Operator CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &operatorCA{cert: cert, key: key}
}

// issue returns a certificate with a new key for 127.0.0.1.
func (ca *operatorCA) issue(t *testing.T, serial int64) *tls.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "hearst"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

func (ca *operatorCA) file(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "operator-ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw}), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOperatorCertificate(t *testing.T) {
	ca := newOperatorCA(t)
	cert := ca.issue(t, 2)
	s := newStackWith(t, &cert)
	stateDir := filepath.Join(t.TempDir(), "skepctl")
	caFile := ca.file(t)
	store := ctlstate.New(stateDir)

	// Without a CA: the self-signed flow would ask about the key. With
	// --system-roots the operator CA is unknown: refused, nothing stored,
	// the password never asked for.
	term := &terminal{passwords: []string{testPassword}}
	_, _, err := skepctl(stateDir, term, "", "login", "--server", s.server(), "--username", "andy", "--system-roots")
	if err == nil || !strings.Contains(err.Error(), "does not verify against the system roots") {
		t.Fatalf("system roots: %v", err)
	}
	if _, ok, _ := store.Trust(s.server()); ok || len(term.prompts) != 0 {
		t.Fatalf("a failed verification stored a trust setting or asked something (%q)", term.prompts)
	}

	// --ca-file: verified, no question, no pin.
	term = &terminal{passwords: []string{testPassword}}
	_, errOut, err := skepctl(stateDir, term, "", "login", "--server", s.server(), "--username", "andy", "--ca-file", caFile)
	if err != nil {
		t.Fatalf("login with --ca-file: %v\n%s", err, errOut)
	}
	if len(term.prompts) != 1 || strings.Contains(term.prompts[0], "Trust this key") {
		t.Errorf("prompts = %q, want only the password", term.prompts)
	}
	tr, _, _ := store.Trust(s.server())
	if !tr.VerifiesChain() || tr.CAFile != caFile || tr.PublicKeySHA256 != "" {
		t.Fatalf("stored trust = %+v", tr)
	}
	if _, _, err := skepctl(stateDir, nil, "", "system"); err != nil {
		t.Fatalf("system: %v", err)
	}

	// A routine renewal with a new key: clients keep working.
	oldPin := client.PublicKeyPin(cert.Leaf)
	cert = ca.issue(t, 3)
	s.srv.CloseClientConnections()
	if _, _, err := skepctl(stateDir, nil, "", "system"); err != nil {
		t.Errorf("system after a renewal with a new key: %v", err)
	}

	// An optional pin is checked in addition, and a stale one refuses.
	_, _, err = skepctl(stateDir, &terminal{passwords: []string{testPassword}}, "",
		"login", "--username", "andy", "--ca-file", caFile, "--trust-key", oldPin)
	if err == nil || !strings.Contains(err.Error(), "nothing was trusted") {
		t.Errorf("--ca-file with a stale pin: %v", err)
	}
	if got, _, _ := store.Trust(s.server()); got != tr {
		t.Errorf("a refused login changed the trust setting: %+v", got)
	}

	// A certificate from another CA: refused, and the error says what to do.
	cert = newOperatorCA(t).issue(t, 4)
	s.srv.CloseClientConnections()
	_, _, err = skepctl(stateDir, nil, "", "system")
	if err == nil || !strings.Contains(err.Error(), "not trusted") || !strings.Contains(err.Error(), "--ca-file") {
		t.Errorf("certificate from another CA: %v", err)
	}
	if _, ok, _ := store.Session(s.server()); !ok {
		t.Error("an untrusted certificate dropped the token")
	}

	// Back to pinning, by naming the key.
	newPin := client.PublicKeyPin(cert.Leaf)
	_, errOut, err = skepctl(stateDir, &terminal{passwords: []string{testPassword}}, "",
		"login", "--username", "andy", "--trust-key", newPin)
	if err != nil || !strings.Contains(errOut, "to a pinned key") {
		t.Fatalf("switching to a pin: %v\n%s", err, errOut)
	}
	if got, _, _ := store.Trust(s.server()); got.VerifiesChain() || got.PublicKeySHA256 != newPin {
		t.Errorf("after switching: %+v", got)
	}

	// And from a pin to chain verification again.
	cert = ca.issue(t, 5)
	s.srv.CloseClientConnections()
	_, errOut, err = skepctl(stateDir, &terminal{passwords: []string{testPassword}}, "",
		"login", "--username", "andy", "--ca-file", caFile)
	if err != nil || !strings.Contains(errOut, "Switching") || !strings.Contains(errOut, "from the pinned key") {
		t.Errorf("switching to chain verification: %v\n%s", err, errOut)
	}

	if _, _, err := skepctl(stateDir, nil, "", "login", "--username", "andy", "--ca-file", caFile, "--system-roots"); err == nil {
		t.Error("--ca-file and --system-roots together were accepted")
	}
	if _, _, err := skepctl(stateDir, &terminal{passwords: []string{testPassword}}, "", "login", "--username", "andy", "--ca-file", filepath.Join(t.TempDir(), "missing.pem")); err == nil {
		t.Error("a missing CA file was accepted")
	}
}

// TestSelfSignedWithChainFlags: a self-signed key is trusted only through
// the pin; --system-roots does not make a self-signed certificate valid.
func TestSelfSignedWithChainFlags(t *testing.T) {
	s := newStack(t)
	stateDir := filepath.Join(t.TempDir(), "skepctl")
	_, _, err := skepctl(stateDir, &terminal{passwords: []string{testPassword}}, "",
		"login", "--server", s.server(), "--username", "andy", "--system-roots")
	if err == nil || !strings.Contains(err.Error(), "does not verify") {
		t.Errorf("self-signed with --system-roots: %v", err)
	}
}

func TestNoTerminalMessage(t *testing.T) {
	s := newStack(t)
	_, _, err := skepctl(filepath.Join(t.TempDir(), "skepctl"), &terminal{}, testPassword+"\n",
		"login", "--server", s.server(), "--username", "andy", "--password-stdin")
	if err == nil || strings.Contains(err.Error(), "password") || !strings.Contains(err.Error(), "no terminal to confirm the key on") {
		t.Errorf("error = %v", err)
	}
}

// TestReloginEndsOnlyItsOwnSession: a new login ends the session whose
// token skepctl stored, and no other session of the user: not a browser's
// cookie session, not another skepctl's bearer session.
func TestReloginEndsOnlyItsOwnSession(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	browser, err := s.authn.Login(ctx, auth.LoginRequest{Username: "andy", Password: testPassword, Kind: auth.CookieSession, SourceIP: "192.0.2.10"})
	if err != nil {
		t.Fatal(err)
	}
	elsewhere, err := s.authn.Login(ctx, auth.LoginRequest{Username: "andy", Password: testPassword, Kind: auth.BearerSession, SourceIP: "192.0.2.11"})
	if err != nil {
		t.Fatal(err)
	}

	stateDir := filepath.Join(t.TempDir(), "skepctl")
	login(t, s, stateDir, "andy")
	first, _, _ := ctlstate.New(stateDir).Session(s.server())
	login(t, s, stateDir, "andy")
	second, _, _ := ctlstate.New(stateDir).Session(s.server())
	if first.Token == second.Token {
		t.Fatal("the second login did not get a new token")
	}

	alive := func(token string, kind auth.SessionKind) bool {
		_, err := s.authn.Authenticate(ctx, token, kind)
		return err == nil
	}
	if alive(first.Token, auth.BearerSession) {
		t.Error("the session skepctl replaced is still alive")
	}
	if !alive(second.Token, auth.BearerSession) {
		t.Error("the new session is not alive")
	}
	if !alive(browser.Token, auth.CookieSession) {
		t.Error("the browser's cookie session was ended by a skepctl login")
	}
	if !alive(elsewhere.Token, auth.BearerSession) {
		t.Error("another bearer session of the user was ended by a skepctl login")
	}
	// Logout is the same: only skepctl's own session.
	if _, _, err := skepctl(stateDir, nil, "", "logout"); err != nil {
		t.Fatal(err)
	}
	if !alive(browser.Token, auth.CookieSession) || !alive(elsewhere.Token, auth.BearerSession) {
		t.Error("logout ended another session of the user")
	}
}
