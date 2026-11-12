package client

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/pkg/models"
)

// testCA is a CA that issues leaf certificates, as an operator's would.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newCA(t *testing.T) *testCA {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Test Operator CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &testCA{cert: cert, key: key}
}

// leaf issues a certificate with a new key for 127.0.0.1 and the given name.
func (ca *testCA) leaf(t *testing.T, serial int64) tls.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "skep.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"skep.test"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

func (ca *testCA) pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.cert)
	return p
}

// operatorServer serves cert, which the test can replace (a renewal).
func operatorServer(t *testing.T, cert *tls.Certificate) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, models.System{NodeID: "node-1"})
	}))
	// Not StartTLS: its own certificate would be served to clients that
	// send no SNI, as clients connecting to an IP address do.
	srv.Listener = tls.NewListener(srv.Listener, &tls.Config{
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return cert, nil },
	})
	srv.Start()
	srv.URL = strings.Replace(srv.URL, "http://", "https://", 1)
	t.Cleanup(srv.Close)
	return srv
}

func TestVerifyChain(t *testing.T) {
	ca := newCA(t)
	cert := ca.leaf(t, 2)
	srv := operatorServer(t, &cert)
	ctx := context.Background()
	system := func(opts Options) error {
		opts.Server = srv.URL
		c, err := New(opts)
		if err != nil {
			return err
		}
		defer c.Close()
		c.http.Transport.(*http.Transport).DisableKeepAlives = true
		_, err = c.System(ctx)
		return err
	}

	// Trusted CA, no pin: works.
	if err := system(Options{VerifyChain: true, RootCAs: ca.pool()}); err != nil {
		t.Fatalf("operator CA: %v", err)
	}
	// A renewal with a new key, from the same CA: still works without a pin.
	oldPin := PublicKeyPin(cert.Leaf)
	cert = ca.leaf(t, 3)
	if PublicKeyPin(cert.Leaf) == oldPin {
		t.Fatal("the renewal kept the key")
	}
	if err := system(Options{VerifyChain: true, RootCAs: ca.pool()}); err != nil {
		t.Errorf("after a renewal with a new key: %v", err)
	}
	// With an optional pin, it is checked in addition.
	if err := system(Options{VerifyChain: true, RootCAs: ca.pool(), Pin: PublicKeyPin(cert.Leaf)}); err != nil {
		t.Errorf("chain and matching pin: %v", err)
	}
	var km *KeyMismatchError
	if err := system(Options{VerifyChain: true, RootCAs: ca.pool(), Pin: oldPin}); !errors.As(err, &km) {
		t.Errorf("chain and stale pin: %v, want a key mismatch", err)
	}
	// Another CA, or the system roots: refused as untrusted.
	if err := system(Options{VerifyChain: true, RootCAs: newCA(t).pool()}); err == nil || !IsUntrustedCertificate(err) {
		t.Errorf("unknown CA: %v", err)
	}
	if err := system(Options{VerifyChain: true}); err == nil || !IsUntrustedCertificate(err) {
		t.Errorf("system roots: %v", err)
	}
	// The host name is checked: the certificate is for 127.0.0.1 and
	// skep.test, not localhost.
	c, _ := New(Options{Server: strings.Replace(srv.URL, "127.0.0.1", "localhost", 1), VerifyChain: true, RootCAs: ca.pool()})
	defer c.Close()
	if _, err := c.System(ctx); err == nil || !IsUntrustedCertificate(err) {
		t.Errorf("wrong host name: %v", err)
	}
	// Neither a pin nor chain verification: no client.
	if _, err := New(Options{Server: srv.URL}); err == nil {
		t.Error("a client without any verification was created")
	}
}

func TestLoadCAFile(t *testing.T) {
	ca := newCA(t)
	dir := t.TempDir()
	good := filepath.Join(dir, "ca.pem")
	os.WriteFile(good, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw}), 0o644)
	if _, err := LoadCAFile(good); err != nil {
		t.Error(err)
	}
	bad := filepath.Join(dir, "bad.pem")
	os.WriteFile(bad, []byte("not a certificate"), 0o644)
	if _, err := LoadCAFile(bad); err == nil {
		t.Error("a file without certificates was accepted")
	}
	if _, err := LoadCAFile(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing file was accepted")
	}
}
