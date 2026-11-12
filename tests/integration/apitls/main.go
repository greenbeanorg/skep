// Command apitls-test is the native integration test for skep-api's TLS
// handling (ADR 0006 decision 6). It runs the real skep-api binary as the
// invoking, unprivileged user, in a scratch data directory, and checks:
// certificate generation at first start, the file modes of the key, TLS 1.3
// only, renewal with the same key on a live server without dropping a
// connection, SIGHUP reload, "tls regenerate" and "tls fingerprint", and an
// operator-supplied certificate with reload and a refused bad pair.
//
// It needs no privileges and changes nothing outside its scratch
// directory. No agent is needed: the API answers /health without one.
package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

var failed int

func check(ok bool, format string, args ...any) bool {
	msg := fmt.Sprintf(format, args...)
	if ok {
		fmt.Printf("ok     %s\n", msg)
	} else {
		failed++
		fmt.Printf("FAIL   %s\n", msg)
	}
	return ok
}

func step(format string, args ...any) { fmt.Printf("\n== %s\n", fmt.Sprintf(format, args...)) }

func fatal(format string, args ...any) {
	fmt.Printf("ABORT  %s\n", fmt.Sprintf(format, args...))
	os.Exit(1)
}

func spki(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// server is one running skep-api.
type server struct {
	cmd  *exec.Cmd
	logs *bytes.Buffer
	addr string
}

func start(bin, addr string, args ...string) *server {
	logs := &bytes.Buffer{}
	cmd := exec.Command(bin, append([]string{"-listen", addr, "-log-format", "json"}, args...)...)
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Start(); err != nil {
		fatal("start %s: %v", bin, err)
	}
	s := &server{cmd: cmd, logs: logs, addr: addr}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			conn.Close()
			return s
		}
		if time.Now().After(deadline) {
			s.stop()
			fatal("skep-api did not start listening on %s:\n%s", addr, logs.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (s *server) stop() {
	_ = s.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { s.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		s.cmd.Process.Kill()
	}
}

func (s *server) hup() {
	if err := s.cmd.Process.Signal(syscall.SIGHUP); err != nil {
		fatal("SIGHUP: %v", err)
	}
}

// peer returns the certificate the server presents to a new connection.
func (s *server) peer(maxVersion uint16) (*x509.Certificate, uint16, error) {
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", s.addr,
		&tls.Config{InsecureSkipVerify: true, MaxVersion: maxVersion}) // the test inspects the certificate itself
	if err != nil {
		return nil, 0, err
	}
	defer conn.Close()
	st := conn.ConnectionState()
	return st.PeerCertificates[0], st.Version, nil
}

// waitPeer waits until new connections see a certificate that satisfies
// cond.
func (s *server) waitPeer(cond func(*x509.Certificate) bool) (*x509.Certificate, bool) {
	deadline := time.Now().Add(15 * time.Second)
	for {
		cert, _, err := s.peer(0)
		if err == nil && cond(cert) {
			return cert, true
		}
		if time.Now().After(deadline) {
			return cert, false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func mode(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("%04o", info.Mode().Perm())
}

// issue writes a self-signed certificate for key to certFile, valid from
// now until notAfter.
func issue(key crypto.Signer, certFile string, names []string, ips []net.IP, notAfter time.Time) *x509.Certificate {
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "apitls-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     names,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		fatal("create certificate: %v", err)
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		fatal("write %s: %v", certFile, err)
	}
	cert, _ := x509.ParseCertificate(der)
	return cert
}

// newPair writes a new key and certificate and returns the certificate.
func newPair(certFile, keyFile string) *x509.Certificate {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		fatal("generate key: %v", err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		fatal("write %s: %v", keyFile, err)
	}
	return issue(key, certFile, []string{"localhost"}, []net.IP{net.IPv4(127, 0, 0, 1)}, time.Now().Add(90*24*time.Hour))
}

func readKey(keyFile string) crypto.Signer {
	b, err := os.ReadFile(keyFile)
	if err != nil {
		fatal("read %s: %v", keyFile, err)
	}
	block, _ := pem.Decode(b)
	if block == nil {
		fatal("%s is not PEM", keyFile)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		fatal("parse %s: %v", keyFile, err)
	}
	return key.(crypto.Signer)
}

type auditRow struct{ Actor, Action, Details string }

func auditRows(dbPath, action string) []auditRow {
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(5000)")
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: dbPath}).EscapedPath()+"?"+q.Encode())
	if err != nil {
		return nil
	}
	defer db.Close()
	rows, err := db.Query(`SELECT actor_name, action, COALESCE(details, '') FROM audit_log WHERE action = ? ORDER BY id`, action)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		if rows.Scan(&r.Actor, &r.Action, &r.Details) == nil {
			out = append(out, r)
		}
	}
	return out
}

func runCmd(bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	return string(out), err
}

func main() {
	bin := flag.String("bin", "./skep-api", "the skep-api binary to test")
	addr := flag.String("addr", "127.0.0.1:18443", "loopback address for the test server")
	keep := flag.Bool("keep", false, "keep the scratch directory")
	flag.Parse()
	if os.Geteuid() == 0 {
		fatal("run this test as an ordinary user: skep-api refuses to run as root")
	}
	fmt.Printf("       %s/%s, uid %d\n", runtime.GOOS, runtime.GOARCH, os.Getuid())
	if out, err := runCmd(*bin, "-version"); err != nil {
		fatal("%s -version: %v", *bin, err)
	} else {
		fmt.Printf("       %s", out)
	}

	scratch, err := os.MkdirTemp("", "skep-apitls")
	if err != nil {
		fatal("%v", err)
	}
	if !*keep {
		defer os.RemoveAll(scratch)
	}
	dataDir := filepath.Join(scratch, "data")
	tlsDir := filepath.Join(dataDir, "api", "tls")
	certFile, keyFile := filepath.Join(tlsDir, "cert.pem"), filepath.Join(tlsDir, "key.pem")
	dbPath := filepath.Join(dataDir, "api", "api.db")
	common := []string{"-dev", "-data-dir", dataDir, "-agent-socket", filepath.Join(scratch, "no-agent.sock")}

	step("first start generates a key and a self-signed certificate")
	srv := start(*bin, *addr, common...)
	defer func() { srv.stop() }()
	check(mode(filepath.Join(dataDir, "api")) == "0700", "state directory mode %s (want 0700)", mode(filepath.Join(dataDir, "api")))
	check(mode(tlsDir) == "0700", "TLS directory mode %s (want 0700)", mode(tlsDir))
	check(mode(keyFile) == "0600", "key file mode %s (want 0600)", mode(keyFile))
	first, version, err := srv.peer(0)
	if err != nil {
		fatal("TLS handshake: %v", err)
	}
	check(version == tls.VersionTLS13, "negotiated TLS version 0x%04x (want 0x0304, TLS 1.3)", version)
	_, _, err = srv.peer(tls.VersionTLS12)
	check(err != nil, "a client limited to TLS 1.2 is refused (%v)", err)
	pub, isECDSA := first.PublicKey.(*ecdsa.PublicKey)
	check(isECDSA && pub.Curve == elliptic.P256(), "the key is ECDSA P-256 (%T)", first.PublicKey)
	days := time.Until(first.NotAfter).Hours() / 24
	check(days > 396.9 && days <= 397, "validity is 397 days (%.2f days left)", days)
	host, _ := os.Hostname()
	check(strings.Contains(strings.Join(first.DNSNames, " "), strings.ToLower(host)) && strings.Contains(strings.Join(first.DNSNames, " "), "localhost"),
		"DNS names %v include the hostname %q and localhost", first.DNSNames, host)
	var ips []string
	for _, ip := range first.IPAddresses {
		ips = append(ips, ip.String())
	}
	want := map[string]bool{}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLinkLocalUnicast() {
				want[ipn.IP.String()] = true
			}
		}
	}
	missing := 0
	for ip := range want {
		if !strings.Contains(" "+strings.Join(ips, " ")+" ", " "+ip+" ") {
			missing++
		}
	}
	check(missing == 0, "IP addresses %v include every address of this host (%d missing)", ips, missing)

	// A verifying client that trusts exactly this certificate.
	pool := x509.NewCertPool()
	pool.AddCert(first)
	hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	resp, err := hc.Get("https://" + *addr + "/api/v1/health")
	if check(err == nil, "a client that trusts the generated certificate verifies it for %s (%v)", *addr, err) {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		check(resp.Header.Get("Strict-Transport-Security") == "", "no Strict-Transport-Security with the self-signed certificate (%q)", resp.Header.Get("Strict-Transport-Security"))
	}
	plain, err := http.Get("http://" + *addr + "/api/v1/health")
	if err == nil {
		plain.Body.Close()
	}
	check(err != nil || plain.StatusCode != http.StatusOK, "plain HTTP gets no API response (err=%v)", err)

	out, err := runCmd(*bin, "tls", "fingerprint", "-dev", "-data-dir", dataDir)
	check(err == nil && strings.Contains(out, spki(first)), "'tls fingerprint' prints the pin the server presents: %s", spki(first))

	step("renewal on a live server: same key, new certificate, no restart")
	// Hold a connection open across the renewal.
	held, err := tls.Dial("tcp", *addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		fatal("dial: %v", err)
	}
	defer held.Close()
	// Put a certificate for the same key on disk that expires in 10 days,
	// as if 387 days had passed, and tell the server to look.
	short := issue(readKey(keyFile), certFile, first.DNSNames, first.IPAddresses, time.Now().Add(10*24*time.Hour))
	pid := srv.cmd.Process.Pid
	srv.hup()
	renewed, ok := srv.waitPeer(func(c *x509.Certificate) bool {
		return c.SerialNumber.Cmp(short.SerialNumber) != 0 && c.SerialNumber.Cmp(first.SerialNumber) != 0
	})
	check(ok, "after SIGHUP the server presents a new certificate (serial %s)", renewed.SerialNumber.Text(16))
	check(spki(renewed) == spki(first), "the public key is unchanged: %s", spki(renewed))
	days = time.Until(renewed.NotAfter).Hours() / 24
	check(days > 396.9 && days <= 397, "the new certificate is valid for 397 days (%.2f)", days)
	check(strings.Join(renewed.DNSNames, ",") == strings.Join(first.DNSNames, ","), "the names are unchanged")
	check(srv.cmd.ProcessState == nil && syscall.Kill(pid, 0) == nil, "skep-api was not restarted (pid %d)", pid)
	_, werr := io.WriteString(held, "GET /api/v1/health HTTP/1.1\r\nHost: localhost\r\n\r\n")
	buf := make([]byte, 9)
	held.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, rerr := io.ReadFull(held, buf)
	check(werr == nil && rerr == nil && string(buf) == "HTTP/1.1 ", "the connection opened before the renewal still works (%q, %v, %v)", buf, werr, rerr)
	onDisk, _ := os.ReadFile(certFile)
	block, _ := pem.Decode(onDisk)
	diskCert, _ := x509.ParseCertificate(block.Bytes)
	check(diskCert != nil && diskCert.SerialNumber.Cmp(renewed.SerialNumber) == 0, "the renewed certificate is the one on disk")
	check(mode(keyFile) == "0600", "key file mode is still %s", mode(keyFile))
	rows := auditRows(dbPath, "tls.certificate.renew")
	check(len(rows) == 1 && rows[0].Actor == "system" && strings.Contains(rows[0].Details, "old_certificate_sha256") &&
		strings.Contains(rows[0].Details, spki(first)), "the renewal is in the access audit log: %+v", rows)

	step("'tls regenerate' creates a new key, audited, and the server picks it up")
	out, err = runCmd(*bin, "tls", "regenerate", "-dev", "-data-dir", dataDir)
	check(err == nil && strings.Contains(out, "old public key: "+spki(first)), "tls regenerate ran: %v", err)
	srv.hup()
	regen, ok := srv.waitPeer(func(c *x509.Certificate) bool { return spki(c) != spki(first) })
	check(ok, "the server presents the new key: %s", spki(regen))
	check(strings.Contains(out, spki(regen)), "it is the key 'tls regenerate' printed")
	check(mode(keyFile) == "0600", "new key file mode %s", mode(keyFile))
	rows = auditRows(dbPath, "tls.key.regenerate")
	wantActor := fmt.Sprintf("local:%d", os.Getuid())
	check(len(rows) == 1 && rows[0].Actor == wantActor && strings.Contains(rows[0].Details, spki(first)) && strings.Contains(rows[0].Details, spki(regen)),
		"the key change is in the access audit log with actor %s and both fingerprints: %+v", wantActor, rows)

	step("a restart keeps the key and the certificate")
	srv.stop()
	srv = start(*bin, *addr, common...)
	again, _, err := srv.peer(0)
	check(err == nil && again.SerialNumber.Cmp(regen.SerialNumber) == 0, "same certificate after a restart (%v)", err)

	step("broken generated files are refused, not replaced")
	srv.stop()
	goodKey, _ := os.ReadFile(keyFile)
	os.WriteFile(keyFile, []byte("not a key\n"), 0o600)
	out, err = runCmd(*bin, append([]string{"-listen", *addr}, common...)...)
	check(err != nil && strings.Contains(out, "tls regenerate"), "skep-api refuses to start and names 'tls regenerate' (%v)", err)
	now, _ := os.ReadFile(keyFile)
	check(string(now) == "not a key\n", "it did not overwrite the key file")
	os.WriteFile(keyFile, goodKey, 0o600)

	step("operator-supplied certificate")
	opDir := filepath.Join(scratch, "operator")
	os.Mkdir(opDir, 0o700)
	opCert, opKey := filepath.Join(opDir, "tls.crt"), filepath.Join(opDir, "tls.key")
	certA := newPair(opCert, opKey)
	cfgFile := filepath.Join(scratch, "api.json")
	cfg, _ := json.Marshal(map[string]any{"tls": map[string]string{"cert_file": opCert, "key_file": opKey}})
	os.WriteFile(cfgFile, cfg, 0o600)
	opData := filepath.Join(scratch, "data-operator")
	opArgs := []string{"-dev", "-config", cfgFile, "-data-dir", opData, "-agent-socket", filepath.Join(scratch, "no-agent.sock")}
	srv = start(*bin, *addr, opArgs...)
	got, _, err := srv.peer(0)
	check(err == nil && got.SerialNumber.Cmp(certA.SerialNumber) == 0, "the server presents the operator's certificate (%v)", err)
	_, statErr := os.Stat(filepath.Join(opData, "api", "tls"))
	check(os.IsNotExist(statErr), "nothing was generated")
	pool = x509.NewCertPool()
	pool.AddCert(certA)
	hc = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	if resp, err := hc.Get("https://" + *addr + "/api/v1/health"); check(err == nil, "verified request (%v)", err) {
		resp.Body.Close()
		check(strings.HasPrefix(resp.Header.Get("Strict-Transport-Security"), "max-age="), "Strict-Transport-Security is sent with an operator certificate (%q)", resp.Header.Get("Strict-Transport-Security"))
	}

	// New files, then SIGHUP.
	certB := newPair(opCert, opKey)
	srv.hup()
	_, ok = srv.waitPeer(func(c *x509.Certificate) bool { return c.SerialNumber.Cmp(certB.SerialNumber) == 0 })
	check(ok, "after SIGHUP the server presents the replaced certificate")

	// A certificate that does not match the key: refused, old one stays.
	otherCert, otherKey := filepath.Join(opDir, "other.crt"), filepath.Join(opDir, "other.key")
	newPair(otherCert, otherKey)
	mismatch, _ := os.ReadFile(otherCert)
	keyB, _ := os.ReadFile(opKey)
	os.WriteFile(opCert, mismatch, 0o644)
	srv.hup()
	time.Sleep(2 * time.Second)
	got, _, err = srv.peer(0)
	check(err == nil && got.SerialNumber.Cmp(certB.SerialNumber) == 0, "a certificate that does not match the key is not loaded; the old one stays")
	check(strings.Contains(srv.logs.String(), "could not be loaded"), "the bad pair is logged")
	nowKey, _ := os.ReadFile(opKey)
	nowCert, _ := os.ReadFile(opCert)
	check(string(nowKey) == string(keyB) && string(nowCert) == string(mismatch), "skep-api did not touch the operator's files")
	out, err = runCmd(*bin, "tls", "regenerate", "-dev", "-config", cfgFile, "-data-dir", opData)
	check(err != nil && strings.Contains(out, "operator-supplied"), "'tls regenerate' refuses to replace operator files")

	// A file change without SIGHUP is picked up by the periodic check.
	certC := newPair(opCert, opKey)
	fmt.Println("       waiting up to 75 s for the periodic check (no SIGHUP)")
	deadline := time.Now().Add(75 * time.Second)
	picked := false
	for time.Now().Before(deadline) {
		if c, _, err := srv.peer(0); err == nil && c.SerialNumber.Cmp(certC.SerialNumber) == 0 {
			picked = true
			break
		}
		time.Sleep(time.Second)
	}
	check(picked, "changed files are picked up without a signal")

	if failed > 0 {
		fmt.Printf("\nFAILED: %d check(s) failed\n", failed)
		srv.stop()
		os.Exit(1)
	}
	fmt.Println("\nPASS")
}
