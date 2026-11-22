package apitls

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// clock is a settable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type auditLog struct {
	mu     sync.Mutex
	events []Event
}

func (a *auditLog) write(_ context.Context, e Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, e)
}

func (a *auditLog) all() []Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Event(nil), a.events...)
}

func newSelfSigned(t *testing.T, dir string, c *clock, a *auditLog) *Manager {
	t.Helper()
	opts := Options{Dir: dir, Logger: quiet(), Now: c.now}
	if a != nil {
		opts.Audit = a.write
	}
	m, err := New(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestGeneratesAtFirstStart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	c := &clock{t: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)}
	m := newSelfSigned(t, dir, c, nil)

	if got := mode(t, dir); got != 0o700 {
		t.Errorf("TLS directory mode = %04o, want 0700", got)
	}
	if got := mode(t, filepath.Join(dir, KeyFileName)); got != 0o600 {
		t.Errorf("key file mode = %04o, want 0600", got)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Errorf("unexpected files in the TLS directory: %v", entries)
	}

	cert, err := m.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	leaf := cert.Leaf
	key, ok := cert.PrivateKey.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		t.Errorf("key is %T, want ECDSA P-256", cert.PrivateKey)
	}
	if got := leaf.NotAfter.Sub(c.now()); got != 397*24*time.Hour {
		t.Errorf("validity = %v, want 397 days", got)
	}
	if leaf.IsCA || len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Errorf("certificate is not a plain server certificate: CA=%v usage=%v", leaf.IsCA, leaf.ExtKeyUsage)
	}
	host, _ := os.Hostname()
	names := strings.Join(leaf.DNSNames, " ")
	if !strings.Contains(names, "localhost") || (host != "" && !strings.Contains(names, strings.ToLower(host))) {
		t.Errorf("DNS names = %v, want the hostname and localhost", leaf.DNSNames)
	}
	hasLoopback := false
	for _, ip := range leaf.IPAddresses {
		if ip.Equal(net.IPv4(127, 0, 0, 1)) {
			hasLoopback = true
		}
		if ip.IsLinkLocalUnicast() {
			t.Errorf("link-local address %v in the certificate", ip)
		}
	}
	if !hasLoopback {
		t.Errorf("IP addresses = %v, want 127.0.0.1 among them", leaf.IPAddresses)
	}

	info := m.Info()
	if info.Source != SelfSigned || !strings.HasPrefix(info.PublicKeySHA256, "sha256:") || len(info.PublicKeySHA256) != 71 {
		t.Errorf("info = %+v", info)
	}

	// A second start uses what is there: same key, same certificate.
	again := newSelfSigned(t, dir, c, nil)
	if again.Info().CertificateSHA256 != info.CertificateSHA256 {
		t.Error("a restart replaced the certificate")
	}
}

func TestRenewsWithTheSameKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	c := &clock{t: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)}
	a := &auditLog{}
	m := newSelfSigned(t, dir, c, a)
	first := m.Info()
	keyBefore, _ := os.ReadFile(filepath.Join(dir, KeyFileName))

	// 31 days before expiry: nothing happens.
	c.advance(Validity - 31*24*time.Hour)
	m.Tick(context.Background())
	if m.Info().CertificateSHA256 != first.CertificateSHA256 || len(a.all()) != 0 {
		t.Fatal("renewed more than 30 days before expiry")
	}

	// 29 days before expiry: a new certificate, the same key.
	c.advance(2 * 24 * time.Hour)
	m.Tick(context.Background())
	second := m.Info()
	if second.CertificateSHA256 == first.CertificateSHA256 {
		t.Fatal("not renewed 29 days before expiry")
	}
	if second.PublicKeySHA256 != first.PublicKeySHA256 {
		t.Error("renewal changed the key; pinned clients would break")
	}
	keyAfter, _ := os.ReadFile(filepath.Join(dir, KeyFileName))
	if string(keyBefore) != string(keyAfter) {
		t.Error("renewal rewrote the key file")
	}
	if got := second.NotAfter.Sub(c.now()); got != Validity {
		t.Errorf("renewed validity = %v", got)
	}
	if strings.Join(second.DNSNames, ",") != strings.Join(first.DNSNames, ",") ||
		strings.Join(second.IPAddresses, ",") != strings.Join(first.IPAddresses, ",") {
		t.Error("renewal changed the names; only regenerate may do that")
	}

	events := a.all()
	if len(events) != 1 || events[0].Action != ActionRenew ||
		events[0].Details["old_certificate_sha256"] != first.CertificateSHA256 ||
		events[0].Details["new_certificate_sha256"] != second.CertificateSHA256 {
		t.Errorf("audit events = %+v", events)
	}

	// The renewed certificate is on disk: a restart serves it.
	if again := newSelfSigned(t, dir, c, nil); again.Info().CertificateSHA256 != second.CertificateSHA256 {
		t.Error("the renewed certificate was not written to disk")
	}
	// And it is not renewed again right away.
	m.Tick(context.Background())
	if len(a.all()) != 1 {
		t.Error("renewed twice")
	}
}

func TestExpiredCertificateIsRenewedAtStart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	c := &clock{t: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)}
	first := newSelfSigned(t, dir, c, nil).Info()
	c.advance(Validity + 24*time.Hour)
	a := &auditLog{}
	m := newSelfSigned(t, dir, c, a)
	if m.Info().PublicKeySHA256 != first.PublicKeySHA256 || !m.Info().NotAfter.After(c.now()) || len(a.all()) != 1 {
		t.Errorf("an expired certificate at start: %+v, events %v", m.Info(), a.all())
	}
}

// testServer is an HTTPS server that uses exactly the manager's
// configuration. (httptest's TLS server adds a certificate of its own.)
type testServer struct{ Listener net.Listener }

func serve(t *testing.T, m *Manager) *testServer {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", m.TLSConfig())
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Handler:  http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }),
		ErrorLog: slog.NewLogLogger(quiet().Handler(), slog.LevelError),
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return &testServer{Listener: ln}
}

// peerCert connects with the given TLS versions and returns the server's
// certificate.
func peerCert(t *testing.T, srv *testServer, minVersion, maxVersion uint16) (*x509.Certificate, error) {
	t.Helper()
	conn, err := tls.Dial("tcp", srv.Listener.Addr().String(), &tls.Config{
		InsecureSkipVerify: true, // the test inspects the certificate itself
		MinVersion:         minVersion,
		MaxVersion:         maxVersion,
	})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return conn.ConnectionState().PeerCertificates[0], nil
}

func TestTLS13OnlyAndSwapWithoutRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	c := &clock{t: time.Now()}
	m := newSelfSigned(t, dir, c, nil)
	srv := serve(t, m)

	if _, err := peerCert(t, srv, tls.VersionTLS12, tls.VersionTLS12); err == nil {
		t.Error("a TLS 1.2 handshake succeeded; the API must be TLS 1.3 only")
	}
	before, err := peerCert(t, srv, tls.VersionTLS13, tls.VersionTLS13)
	if err != nil {
		t.Fatal(err)
	}

	// A connection opened before the renewal keeps working after it.
	held, err := tls.Dial("tcp", srv.Listener.Addr().String(), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	c.advance(Validity - 24*time.Hour)
	m.Tick(context.Background())
	after, err := peerCert(t, srv, tls.VersionTLS13, tls.VersionTLS13)
	if err != nil {
		t.Fatal(err)
	}
	if after.SerialNumber.Cmp(before.SerialNumber) == 0 {
		t.Error("new handshakes still get the old certificate")
	}
	if string(after.RawSubjectPublicKeyInfo) != string(before.RawSubjectPublicKeyInfo) {
		t.Error("the key changed")
	}
	if _, err := io.WriteString(held, "GET / HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatalf("the connection from before the renewal broke: %v", err)
	}
	buf := make([]byte, 12)
	held.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(held, buf); err != nil || string(buf) != "HTTP/1.1 200" {
		t.Errorf("the connection from before the renewal: %q, %v", buf, err)
	}
}

func TestBrokenGeneratedFilesAreNotReplaced(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	c := &clock{t: time.Now()}
	newSelfSigned(t, dir, c, nil)
	keyFile := filepath.Join(dir, KeyFileName)
	original, _ := os.ReadFile(keyFile)

	for name, breakIt := range map[string]func(){
		"garbage key":     func() { os.WriteFile(keyFile, []byte("not a key"), 0o600) },
		"missing key":     func() { os.Remove(keyFile) },
		"key of another":  func() { k, _ := newKey(); b, _ := encodeKey(k); os.WriteFile(keyFile, b, 0o600) },
		"missing cert":    func() { os.Remove(filepath.Join(dir, CertFileName)) },
		"empty directory": func() {},
	} {
		if name == "empty directory" {
			continue
		}
		breakIt()
		_, err := New(context.Background(), Options{Dir: dir, Logger: quiet(), Now: c.now})
		if err == nil || !strings.Contains(err.Error(), "tls regenerate") {
			t.Errorf("%s: error = %v, want a refusal that names 'tls regenerate'", name, err)
		}
		// Restore for the next case.
		os.WriteFile(keyFile, original, 0o600)
		if _, err := os.Stat(filepath.Join(dir, CertFileName)); err != nil {
			Regenerate(dir, c.now(), nil)
			original, _ = os.ReadFile(keyFile)
		}
	}
}

func TestRegenerateMakesANewKeyAndRunningServerPicksItUp(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	c := &clock{t: time.Now()}
	m := newSelfSigned(t, dir, c, nil)
	old := m.Info()

	before, after, err := Regenerate(dir, c.now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if before.PublicKeySHA256 != old.PublicKeySHA256 || after.PublicKeySHA256 == old.PublicKeySHA256 {
		t.Errorf("regenerate: before %s after %s, was %s", before.PublicKeySHA256, after.PublicKeySHA256, old.PublicKeySHA256)
	}
	if got := mode(t, filepath.Join(dir, KeyFileName)); got != 0o600 {
		t.Errorf("new key file mode = %04o", got)
	}
	if m.Info().PublicKeySHA256 != old.PublicKeySHA256 {
		t.Fatal("the running manager changed before its next tick")
	}
	m.Tick(context.Background())
	if m.Info().PublicKeySHA256 != after.PublicKeySHA256 {
		t.Error("the running manager did not pick up the regenerated key")
	}
	fp, err := Fingerprint(dir, "", "")
	if err != nil || fp.PublicKeySHA256 != after.PublicKeySHA256 {
		t.Errorf("Fingerprint = %+v, %v", fp, err)
	}

	// On an empty directory there is no "before".
	empty := filepath.Join(t.TempDir(), "tls")
	before, after, err = Regenerate(empty, c.now(), nil)
	if err != nil || before.PublicKeySHA256 != "" || after.PublicKeySHA256 == "" {
		t.Errorf("regenerate in an empty directory: %+v, %+v, %v", before, after, err)
	}
	if _, err := Fingerprint(filepath.Join(t.TempDir(), "none"), "", ""); err == nil {
		t.Error("Fingerprint without a certificate must fail")
	}
}

// writePair writes a fresh self-signed pair to dir and returns its files.
func writePair(t *testing.T, dir, name string, now time.Time) (certFile, keyFile string, info Info) {
	t.Helper()
	key, err := newKey()
	if err != nil {
		t.Fatal(err)
	}
	der, err := selfSign(key, []string{name}, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, _ := encodeKey(key)
	certFile, keyFile = filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
	if err := os.WriteFile(certFile, encodeCert(der), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return certFile, keyFile, infoFor(Operator, leaf)
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestOperatorCertificate(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: time.Now()}
	certA, keyA, infoA := writePair(t, dir, "a", c.now())
	certB, keyB, infoB := writePair(t, dir, "b", c.now())
	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	copyFile(t, certA, certFile)
	copyFile(t, keyA, keyFile)

	a := &auditLog{}
	tlsDir := filepath.Join(dir, "generated")
	m, err := New(context.Background(), Options{Dir: tlsDir, CertFile: certFile, KeyFile: keyFile, Logger: quiet(), Now: c.now, Audit: a.write})
	if err != nil {
		t.Fatal(err)
	}
	if m.Source() != Operator || m.Info().CertificateSHA256 != infoA.CertificateSHA256 {
		t.Fatalf("info = %+v", m.Info())
	}
	if _, err := os.Stat(tlsDir); err == nil {
		t.Error("a certificate was generated although the operator supplied one")
	}

	// The files change on disk: picked up at the next tick.
	copyFile(t, certB, certFile)
	copyFile(t, keyB, keyFile)
	m.Tick(context.Background())
	if m.Info().CertificateSHA256 != infoB.CertificateSHA256 {
		t.Error("changed operator files were not picked up")
	}

	// A pair that does not match is refused and the old one stays, for a
	// tick and for an explicit reload.
	copyFile(t, certA, certFile)
	m.Tick(context.Background())
	if err := m.Reload(context.Background()); err == nil {
		t.Error("Reload accepted a certificate that does not match the key")
	}
	if m.Info().CertificateSHA256 != infoB.CertificateSHA256 {
		t.Error("a mismatched pair replaced the certificate in use")
	}
	copyFile(t, keyA, keyFile)
	if err := m.Reload(context.Background()); err != nil || m.Info().CertificateSHA256 != infoA.CertificateSHA256 {
		t.Errorf("Reload of a good pair: %v, %+v", err, m.Info())
	}

	// Close to expiry Skep warns and touches nothing.
	certBefore, _ := os.ReadFile(certFile)
	c.advance(Validity - 24*time.Hour)
	m.Tick(context.Background())
	certAfter, _ := os.ReadFile(certFile)
	if string(certBefore) != string(certAfter) || m.Info().CertificateSHA256 != infoA.CertificateSHA256 || len(a.all()) != 0 {
		t.Error("Skep renewed or rewrote an operator-supplied certificate")
	}

	fp, err := Fingerprint(tlsDir, certFile, keyFile)
	if err != nil || fp.Source != Operator || fp.PublicKeySHA256 != infoA.PublicKeySHA256 {
		t.Errorf("Fingerprint = %+v, %v", fp, err)
	}
}

func TestOperatorCertificateErrors(t *testing.T) {
	dir := t.TempDir()
	certA, keyA, _ := writePair(t, dir, "a", time.Now())
	_, keyB, _ := writePair(t, dir, "b", time.Now())
	for name, opts := range map[string]Options{
		"only a certificate": {CertFile: certA},
		"only a key":         {KeyFile: keyA},
		"missing files":      {CertFile: filepath.Join(dir, "none.crt"), KeyFile: filepath.Join(dir, "none.key")},
		"mismatched pair":    {CertFile: certA, KeyFile: keyB},
		"no directory":       {},
	} {
		opts.Logger = quiet()
		if _, err := New(context.Background(), opts); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRunReloadsOnSignal(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	c := &clock{t: time.Now()}
	m := newSelfSigned(t, dir, c, nil)
	old := m.Info()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hup := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		m.Run(ctx, time.Hour, hup)
		close(done)
	}()

	if _, _, err := Regenerate(dir, c.now(), nil); err != nil {
		t.Fatal(err)
	}
	hup <- struct{}{}
	deadline := time.Now().Add(5 * time.Second)
	for m.Info().PublicKeySHA256 == old.PublicKeySHA256 {
		if time.Now().After(deadline) {
			t.Fatal("the signal did not reload the certificate")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

// pendingPair writes a new pair under the pending names, as the first
// step of generate does, and returns its public key pin.
func pendingPair(t *testing.T, dir string, now time.Time) string {
	t.Helper()
	key, err := newKey()
	if err != nil {
		t.Fatal(err)
	}
	der, err := selfSign(key, []string{"next"}, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := encodeKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePending(dir, keyPEM, encodeCert(der)); err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return fingerprint(leaf.RawSubjectPublicKeyInfo)
}

// halfSwapped leaves dir as generate leaves it between its two renames:
// the new key in place, the old certificate still in place, and the new
// certificate pending.
func halfSwapped(t *testing.T, dir string, now time.Time) (newPin string) {
	t.Helper()
	newPin = pendingPair(t, dir, now)
	_, keyNext := pendingFiles(dir)
	if err := os.Rename(keyNext, filepath.Join(dir, KeyFileName)); err != nil {
		t.Fatal(err)
	}
	// This is the window: the files in place do not match.
	if _, err := loadPair(filepath.Join(dir, CertFileName), filepath.Join(dir, KeyFileName)); err == nil {
		t.Fatal("the half-swapped files match; the test does not reach the window")
	}
	return newPin
}

// assertSwapped checks that dir holds a matching pair with the given key
// and no pending files.
func assertSwapped(t *testing.T, dir, wantPin string) {
	t.Helper()
	l, err := loadPair(filepath.Join(dir, CertFileName), filepath.Join(dir, KeyFileName))
	if err != nil {
		t.Fatalf("the files on disk do not match: %v", err)
	}
	if got := fingerprint(l.leaf.RawSubjectPublicKeyInfo); got != wantPin {
		t.Errorf("key on disk %s, want %s", got, wantPin)
	}
	if pendingExists(dir) {
		t.Error("pending files remain after the swap")
	}
	if got := mode(t, filepath.Join(dir, KeyFileName)); got != 0o600 {
		t.Errorf("key file mode = %04o", got)
	}
}

// TestReloadBetweenTheRenames: a reload that lands between generate's
// rename of the key and of the certificate gets the new pair, never a
// mismatched one, and leaves the files on disk matching.
func TestReloadBetweenTheRenames(t *testing.T) {
	for name, reload := range map[string]func(*Manager){
		"tick":   func(m *Manager) { m.Tick(context.Background()) },
		"SIGHUP": func(m *Manager) { _ = m.Reload(context.Background()) },
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "tls")
			c := &clock{t: time.Now()}
			m := newSelfSigned(t, dir, c, nil)
			old := m.Info().PublicKeySHA256

			newPin := halfSwapped(t, dir, c.now())
			reload(m)
			if got := m.Info().PublicKeySHA256; got != newPin || got == old {
				t.Errorf("serving %s after the reload, want the new key %s (old %s)", got, newPin, old)
			}
			cert, _ := m.GetCertificate(nil)
			if cert.Leaf == nil || fingerprint(cert.Leaf.RawSubjectPublicKeyInfo) != newPin {
				t.Error("the served certificate is not the new one")
			}
			assertSwapped(t, dir, newPin)
		})
	}
}

// TestStartAfterACrashBetweenTheRenames: a crash between the renames
// leaves mismatched files; the next start finishes the swap instead of
// refusing to start.
func TestStartAfterACrashBetweenTheRenames(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	c := &clock{t: time.Now()}
	newSelfSigned(t, dir, c, nil)
	newPin := halfSwapped(t, dir, c.now())

	m, err := New(context.Background(), Options{Dir: dir, Logger: quiet(), Now: c.now})
	if err != nil {
		t.Fatalf("start after a crash between the renames: %v", err)
	}
	if got := m.Info().PublicKeySHA256; got != newPin {
		t.Errorf("serving %s, want %s", got, newPin)
	}
	assertSwapped(t, dir, newPin)

	// The same at the very first generation: no old certificate at all.
	first := filepath.Join(t.TempDir(), "tls")
	if err := ensureDir(first); err != nil {
		t.Fatal(err)
	}
	firstPin := pendingPair(t, first, c.now())
	_, keyNext := pendingFiles(first)
	if err := os.Rename(keyNext, filepath.Join(first, KeyFileName)); err != nil {
		t.Fatal(err)
	}
	if m, err = New(context.Background(), Options{Dir: first, Logger: quiet(), Now: c.now}); err != nil {
		t.Fatalf("start after a crash in the first generation: %v", err)
	}
	if m.Info().PublicKeySHA256 != firstPin {
		t.Errorf("serving %s, want %s", m.Info().PublicKeySHA256, firstPin)
	}
	assertSwapped(t, first, firstPin)
}

// TestPendingPairIsNotInstalledOverAWorkingOne: a crash before the
// renames leaves the old pair intact; it stays in use, and the leftover
// pending files do not stop the next regenerate.
func TestPendingPairIsNotInstalledOverAWorkingOne(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	c := &clock{t: time.Now()}
	old := newSelfSigned(t, dir, c, nil).Info().PublicKeySHA256
	pendingPair(t, dir, c.now())

	m, err := New(context.Background(), Options{Dir: dir, Logger: quiet(), Now: c.now})
	if err != nil || m.Info().PublicKeySHA256 != old {
		t.Fatalf("start with a working pair and a pending one: %v, serving %s, want %s", err, m.Info().PublicKeySHA256, old)
	}
	m.Tick(context.Background())
	if m.Info().PublicKeySHA256 != old {
		t.Error("a tick installed the pending pair over a working one")
	}
	_, after, err := Regenerate(dir, c.now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	assertSwapped(t, dir, after.PublicKeySHA256)
}

// TestMismatchedPendingPairIsRefused: a pending key without its own
// certificate is never moved into place.
func TestMismatchedPendingPairIsRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	c := &clock{t: time.Now()}
	newSelfSigned(t, dir, c, nil)
	keyFile := filepath.Join(dir, KeyFileName)
	before, _ := os.ReadFile(keyFile)

	// Break the pair in place, and leave a pending key whose certificate
	// belongs to another key.
	pendingPair(t, dir, c.now())
	other, _ := newKey()
	otherPEM, _ := encodeKey(other)
	_, keyNext := pendingFiles(dir)
	if err := os.WriteFile(keyNext, otherPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(dir, CertFileName))

	_, err := New(context.Background(), Options{Dir: dir, Logger: quiet(), Now: c.now})
	if err == nil || !strings.Contains(err.Error(), "tls regenerate") || !strings.Contains(err.Error(), "pending") {
		t.Errorf("error = %v, want a refusal naming the pending pair and 'tls regenerate'", err)
	}
	if after, _ := os.ReadFile(keyFile); string(after) != string(before) {
		t.Error("the key in place was replaced by a pending key without its certificate")
	}
}

// TestConcurrentReadsNeverSeeAMismatch: while the key is regenerated
// again and again, every load of the generated pair succeeds.
func TestConcurrentReadsNeverSeeAMismatch(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	c := &clock{t: time.Now()}
	m := newSelfSigned(t, dir, c, nil)

	const rounds = 200
	done := make(chan error, 1)
	go func() {
		for i := 0; i < rounds; i++ {
			if _, _, err := Regenerate(dir, c.now(), nil); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	reads := 0
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("regenerate: %v", err)
			}
			if reads == 0 {
				t.Fatal("no read overlapped the regenerations")
			}
			m.Tick(context.Background())
			fp, err := Fingerprint(dir, "", "")
			if err != nil || m.Info().PublicKeySHA256 != fp.PublicKeySHA256 {
				t.Errorf("after the last regenerate: serving %s, on disk %+v (%v)", m.Info().PublicKeySHA256, fp, err)
			}
			assertSwapped(t, dir, fp.PublicKeySHA256)
			t.Logf("%d reads during %d regenerations", reads, rounds)
			return
		default:
		}
		if _, err := loadGenerated(dir); err != nil {
			t.Fatalf("read %d saw an unusable pair: %v", reads, err)
		}
		reads++
	}
}

// TestRegenerateCallsIntentBeforeAnythingChanges: the intent sees what is
// there and exactly the key that will be installed, while nothing on disk
// has changed yet; a failing intent changes nothing.
func TestRegenerateCallsIntentBeforeAnythingChanges(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	c := &clock{t: time.Now()}
	old := newSelfSigned(t, dir, c, nil).Info()
	keyFile := filepath.Join(dir, KeyFileName)
	keyBefore, _ := os.ReadFile(keyFile)

	var seenBefore, seenNext Info
	before, after, err := Regenerate(dir, c.now(), func(b, n Info) error {
		seenBefore, seenNext = b, n
		if k, _ := os.ReadFile(keyFile); string(k) != string(keyBefore) {
			t.Error("the key on disk changed before the intent was recorded")
		}
		if pendingExists(dir) {
			t.Error("pending files were written before the intent was recorded")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seenBefore.PublicKeySHA256 != old.PublicKeySHA256 || before.PublicKeySHA256 != seenBefore.PublicKeySHA256 {
		t.Errorf("intent saw before %s, want %s", seenBefore.PublicKeySHA256, old.PublicKeySHA256)
	}
	if seenNext.PublicKeySHA256 != after.PublicKeySHA256 || seenNext.CertificateSHA256 != after.CertificateSHA256 {
		t.Errorf("intent announced %s, installed %s", seenNext.PublicKeySHA256, after.PublicKeySHA256)
	}

	// A failing intent: nothing changes, nothing is pending.
	keyBefore, _ = os.ReadFile(keyFile)
	refused := errors.New("audit log unavailable")
	if _, _, err := Regenerate(dir, c.now(), func(Info, Info) error { return refused }); !errors.Is(err, refused) {
		t.Fatalf("error = %v, want the intent's", err)
	}
	if k, _ := os.ReadFile(keyFile); string(k) != string(keyBefore) {
		t.Error("the key changed although the intent failed")
	}
	if pendingExists(dir) {
		t.Error("pending files remain after a failed intent")
	}
	assertSwapped(t, dir, after.PublicKeySHA256)
}

// TestReadersWaitForTheSwap: a reader never reads while a swap holds the
// directory lock, even at the moment between the two renames; it waits
// and then gets the new pair.
func TestReadersWaitForTheSwap(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	c := &clock{t: time.Now()}
	newSelfSigned(t, dir, c, nil)

	unlock, err := lockDir(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	newPin := halfSwapped(t, dir, c.now())
	got := make(chan string, 1)
	go func() {
		l, err := loadGenerated(dir)
		if err != nil {
			got <- "error: " + err.Error()
			return
		}
		got <- fingerprint(l.leaf.RawSubjectPublicKeyInfo)
	}()
	select {
	case pin := <-got:
		unlock()
		t.Fatalf("a read finished while the swap held the lock: %s", pin)
	case <-time.After(200 * time.Millisecond):
	}
	if err := finishSwap(dir); err != nil {
		t.Fatal(err)
	}
	unlock()
	select {
	case pin := <-got:
		if pin != newPin {
			t.Errorf("read %s after the swap, want %s", pin, newPin)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the read did not finish after the lock was released")
	}
	assertSwapped(t, dir, newPin)
}

// TestRenewDoesNotPairTheOldKeyWithANewOne: a renewal that runs after
// another process regenerated the key, before the server reloaded, must
// not write a certificate for the old key next to the new key.
func TestRenewDoesNotPairTheOldKeyWithANewOne(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	c := &clock{t: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)}
	a := &auditLog{}
	m := newSelfSigned(t, dir, c, a)
	old := m.Info()

	_, after, err := Regenerate(dir, c.now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	c.advance(Validity - 29*24*time.Hour)
	m.checkExpiry(context.Background()) // no reload first: the race
	if m.Info().CertificateSHA256 != old.CertificateSHA256 || len(a.all()) != 0 {
		t.Error("renewed the old key's certificate after the key was regenerated")
	}
	assertSwapped(t, dir, after.PublicKeySHA256)

	// The next tick picks up the new pair.
	m.Tick(context.Background())
	if m.Info().PublicKeySHA256 != after.PublicKeySHA256 {
		t.Errorf("serving %s after a tick, want %s", m.Info().PublicKeySHA256, after.PublicKeySHA256)
	}
	assertSwapped(t, dir, after.PublicKeySHA256)
}
