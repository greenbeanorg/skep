// Package apitls manages the TLS certificate of skep-api (ADR 0006
// decision 6).
//
// There are two sources. An operator-supplied certificate and key are used
// when configured; Skep reads them, reloads them when they change, and
// never writes to them. Otherwise skep-api generates a self-signed
// certificate at first start and renews it before it expires, always with
// the same key. A new key is generated only by an explicit command.
//
// The private key is a plaintext file readable only by the service
// account. An unattended service has to read its key at boot, so there is
// no other form; the agent never sees it.
package apitls

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Parameters of the generated certificate.
const (
	// Validity is how long a generated certificate is valid: 397 days,
	// the longest lifetime browsers accept for a server certificate.
	Validity = 397 * 24 * time.Hour
	// RenewBefore is how long before expiry a generated certificate is
	// renewed, and from when an operator-supplied one is warned about.
	RenewBefore = 30 * 24 * time.Hour
	// backdate allows for clocks that are a little behind.
	backdate = 5 * time.Minute

	// CertFileName and KeyFileName are the files in the TLS directory.
	CertFileName = "cert.pem"
	KeyFileName  = "key.pem"
)

// Source says where the certificate in use comes from.
type Source string

// Sources.
const (
	SelfSigned Source = "self-signed"
	Operator   Source = "operator"
)

// Info describes the certificate in use. Nothing in it is secret.
type Info struct {
	Source    Source
	NotBefore time.Time
	NotAfter  time.Time
	// PublicKeySHA256 is the SHA-256 of the certificate's
	// SubjectPublicKeyInfo, as "sha256:<hex>". It stays the same across
	// renewals and is what clients pin.
	PublicKeySHA256 string
	// CertificateSHA256 is the SHA-256 of the whole certificate.
	CertificateSHA256 string
	DNSNames          []string
	IPAddresses       []string
}

func fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func infoFor(source Source, leaf *x509.Certificate) Info {
	info := Info{
		Source:            source,
		NotBefore:         leaf.NotBefore,
		NotAfter:          leaf.NotAfter,
		PublicKeySHA256:   fingerprint(leaf.RawSubjectPublicKeyInfo),
		CertificateSHA256: fingerprint(leaf.Raw),
		DNSNames:          append([]string(nil), leaf.DNSNames...),
	}
	for _, ip := range leaf.IPAddresses {
		info.IPAddresses = append(info.IPAddresses, ip.String())
	}
	return info
}

// HostNames returns the subject alternative names for a certificate
// generated on this host now: the hostname, its short form, "localhost",
// and every address configured on an interface. No DNS query is made; the
// fully qualified name is included when the hostname itself is one.
func HostNames() (names []string, ips []net.IP) {
	seen := map[string]bool{}
	add := func(name string) {
		name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
		if name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	if host, err := os.Hostname(); err == nil {
		add(host)
		if short, _, ok := strings.Cut(host, "."); ok {
			add(short)
		}
	}
	add("localhost")

	seenIP := map[string]bool{}
	addIP := func(ip net.IP) {
		if ip == nil || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() {
			return
		}
		if v4 := ip.To4(); v4 != nil {
			ip = v4
		}
		if !seenIP[ip.String()] {
			seenIP[ip.String()] = true
			ips = append(ips, ip)
		}
	}
	addIP(net.IPv4(127, 0, 0, 1))
	addIP(net.IPv6loopback)
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				addIP(ipn.IP)
			}
		}
	}
	return names, ips
}

// newKey generates an ECDSA P-256 key.
func newKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

// selfSign issues a self-signed server certificate for key.
func selfSign(key crypto.Signer, names []string, ips []net.IP, now time.Time) ([]byte, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, fmt.Errorf("apitls: serial number: %w", err)
	}
	cn := "skep"
	if len(names) > 0 {
		cn = names[0]
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"Skep self-signed"}},
		NotBefore:             now.Add(-backdate).UTC(),
		NotAfter:              now.Add(Validity).UTC(),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              names,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, fmt.Errorf("apitls: create certificate: %w", err)
	}
	return der, nil
}

func encodeCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func encodeKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("apitls: encode key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// writeFile replaces path atomically with data and the given mode.
func writeFile(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	// CreateTemp makes the file 0600, so a key is never readable by
	// anyone else, not even for a moment.
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
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
	return os.Rename(tmp.Name(), path)
}

// loaded is a certificate ready to serve, with what was read from disk.
type loaded struct {
	cert *tls.Certificate
	leaf *x509.Certificate
	// stamp identifies the files' state, to notice changes.
	stamp string
}

// loadPair reads a certificate and key and checks that they belong
// together.
func loadPair(certFile, keyFile string) (*loaded, error) {
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, err
	}
	// X509KeyPair fails when the key does not match the certificate.
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("%s and %s: %w", certFile, keyFile, err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("%s: %w", certFile, err)
	}
	cert.Leaf = leaf
	return &loaded{cert: &cert, leaf: leaf, stamp: fileStamp(certPEM, keyPEM)}, nil
}

// fileStamp fingerprints file contents. It is compared, never shown.
func fileStamp(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		sum := sha256.Sum256(p)
		h.Write(sum[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// stampOf reads the two files and fingerprints them, or returns "" when
// they cannot be read.
func stampOf(certFile, keyFile string) string {
	certPEM, err1 := os.ReadFile(certFile)
	keyPEM, err2 := os.ReadFile(keyFile)
	if err1 != nil || err2 != nil {
		return ""
	}
	return fileStamp(certPEM, keyPEM)
}

// samePublicKey reports whether two certificates carry the same key.
func samePublicKey(a, b *x509.Certificate) bool {
	return bytes.Equal(a.RawSubjectPublicKeyInfo, b.RawSubjectPublicKeyInfo)
}

// errNoCertificate means the TLS directory holds neither file yet.
var errNoCertificate = errors.New("apitls: no certificate has been generated yet")

// ensureDir creates the TLS directory, private to the service account.
func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("apitls: create %s: %w", dir, err)
	}
	return nil
}

// generate creates a new key and certificate in dir, replacing what is
// there, so that no reader ever has to use a key and certificate that do
// not match. Two files cannot be renamed in one step, so the swap is made
// atomic for readers with a lock, and recoverable after a crash:
//
//  1. An exclusive lock on dir is taken (lockDir). Readers take a shared
//     one, so none reads while the steps below run, in this process or
//     in another.
//  2. Both new files are written under their pending names
//     (key.pem.next, cert.pem.next), each to a temporary file, synced,
//     and renamed; then the directory is synced. Nothing a reader uses
//     has changed yet, and the complete new pair is durable.
//  3. finishSwap renames the key into place, then the certificate, and
//     syncs the directory. Then the lock is released.
//
// A reader therefore sees the old pair or the new one, never the state
// between the two renames. That state is left on disk only by a crash
// between them; the next reader that finds it (loadGenerated) takes the
// exclusive lock, sees the pending certificate that matches the key, and
// finishes the swap.
func generate(dir string, now time.Time) (*loaded, error) {
	p, err := newPair(now)
	if err != nil {
		return nil, err
	}
	return install(dir, p)
}

// pair is a new key and certificate, in memory only.
type pair struct {
	keyPEM, certPEM []byte
	leaf            *x509.Certificate
}

// newPair creates a key and a self-signed certificate for the host's
// current names and addresses, without writing anything.
func newPair(now time.Time) (*pair, error) {
	key, err := newKey()
	if err != nil {
		return nil, fmt.Errorf("apitls: generate key: %w", err)
	}
	names, ips := HostNames()
	der, err := selfSign(key, names, ips, now)
	if err != nil {
		return nil, err
	}
	keyPEM, err := encodeKey(key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &pair{keyPEM: keyPEM, certPEM: encodeCert(der), leaf: leaf}, nil
}

// install writes p as the pending pair and swaps it into place; see
// generate.
func install(dir string, p *pair) (*loaded, error) {
	if err := ensureDir(dir); err != nil {
		return nil, err
	}
	unlock, err := lockDir(dir, true)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := writePending(dir, p.keyPEM, p.certPEM); err != nil {
		return nil, err
	}
	if err := finishSwap(dir); err != nil {
		return nil, err
	}
	return loadPair(filepath.Join(dir, CertFileName), filepath.Join(dir, KeyFileName))
}

// pendingSuffix marks the files of a new pair that is not in place yet.
const pendingSuffix = ".next"

// pendingFiles returns the pending names of the certificate and key.
func pendingFiles(dir string) (certNext, keyNext string) {
	return filepath.Join(dir, CertFileName+pendingSuffix), filepath.Join(dir, KeyFileName+pendingSuffix)
}

// writePending writes a complete new pair under the pending names and
// makes it durable. The files in use are not touched.
func writePending(dir string, keyPEM, certPEM []byte) error {
	certNext, keyNext := pendingFiles(dir)
	if err := writeFile(keyNext, keyPEM, 0o600); err != nil {
		return fmt.Errorf("apitls: write key: %w", err)
	}
	if err := writeFile(certNext, certPEM, 0o644); err != nil {
		return fmt.Errorf("apitls: write certificate: %w", err)
	}
	return syncDir(dir)
}

// finishSwap moves a pending pair into place: the key first, then the
// certificate. It is idempotent. The caller holds the exclusive lock on
// dir: each check below and the rename after it must see the same file,
// which only holds while no one else can write a pending file.
//
// It never installs a key without its certificate: the pending key moves
// only if it matches the pending certificate, and the pending certificate
// moves only if it matches the key in place.
func finishSwap(dir string) error {
	certFile, keyFile := filepath.Join(dir, CertFileName), filepath.Join(dir, KeyFileName)
	certNext, keyNext := pendingFiles(dir)

	if _, err := os.Stat(keyNext); err == nil {
		_, err := loadPair(certNext, keyNext)
		switch {
		case errors.Is(err, os.ErrNotExist):
			// Moved by someone else since the Stat.
		case err != nil:
			return fmt.Errorf("apitls: the pending key and certificate in %s do not match: %w", dir, err)
		default:
			if err := os.Rename(keyNext, keyFile); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("apitls: install key: %w", err)
			}
		}
	}
	if _, err := os.Stat(certNext); err == nil {
		if _, err := loadPair(certNext, keyFile); err == nil {
			if err := os.Rename(certNext, certFile); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("apitls: install certificate: %w", err)
			}
		}
	}
	return syncDir(dir)
}

// syncDir makes the renames in dir durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("apitls: sync %s: %w", dir, err)
	}
	return nil
}

// loadGenerated loads the generated pair from dir. It returns
// errNoCertificate when neither file exists.
//
// It reads under a shared lock on dir, so it never sees a swap by
// generate half done. When the pair does not load, it takes the
// exclusive lock and reads again; if the pair still does not load and a
// pending pair exists, a swap was interrupted by a crash, and it is
// finished here before loading once more; see generate. Nothing else is
// ever replaced: without a pending pair that matches, the error stands.
func loadGenerated(dir string) (*loaded, error) {
	l, err := readGeneratedShared(dir)
	if err == nil || errors.Is(err, errNoCertificate) {
		return l, err
	}
	unlock, lockErr := lockDir(dir, true)
	if lockErr != nil {
		return nil, lockErr
	}
	defer unlock()
	if l, err = readGenerated(dir); err == nil || errors.Is(err, errNoCertificate) || !pendingExists(dir) {
		return l, err
	}
	if swapErr := finishSwap(dir); swapErr != nil {
		return nil, fmt.Errorf("%w (and %w)", err, swapErr)
	}
	return readGenerated(dir)
}

// readGeneratedShared is readGenerated under a shared lock on dir. A
// directory that does not exist holds no certificate.
func readGeneratedShared(dir string) (*loaded, error) {
	unlock, err := lockDir(dir, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errNoCertificate
	}
	if err != nil {
		return nil, err
	}
	defer unlock()
	return readGenerated(dir)
}

// readGenerated reads the pair in dir; the caller holds a lock on dir.
func readGenerated(dir string) (*loaded, error) {
	certFile, keyFile := filepath.Join(dir, CertFileName), filepath.Join(dir, KeyFileName)
	_, certErr := os.Stat(certFile)
	_, keyErr := os.Stat(keyFile)
	if errors.Is(certErr, os.ErrNotExist) && errors.Is(keyErr, os.ErrNotExist) {
		return nil, errNoCertificate
	}
	l, err := loadPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("apitls: the generated certificate in %s is unusable: %w", dir, err)
	}
	return l, nil
}

// pendingExists reports whether either pending file is in dir.
func pendingExists(dir string) bool {
	certNext, keyNext := pendingFiles(dir)
	_, certErr := os.Stat(certNext)
	_, keyErr := os.Stat(keyNext)
	return certErr == nil || keyErr == nil
}

// keyFileHolds checks that keyFile holds the private key of leaf.
func keyFileHolds(keyFile string, leaf *x509.Certificate) error {
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return fmt.Errorf("apitls: renew: %w", err)
	}
	// X509KeyPair checks that the key belongs to the certificate.
	if _, err := tls.X509KeyPair(encodeCert(leaf.Raw), keyPEM); err != nil {
		return fmt.Errorf("apitls: renew: %s no longer holds the key of the certificate in use (regenerated?); not renewing: %w", keyFile, err)
	}
	return nil
}

// renew issues a new certificate for the existing key, with the names of
// the old certificate, and replaces the certificate file under the
// exclusive lock, after checking that the key file still holds that key.
// The key file is not touched.
func renew(dir string, old *loaded, now time.Time) (*loaded, error) {
	signer, ok := old.cert.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, errors.New("apitls: the private key cannot sign")
	}
	der, err := selfSign(signer, old.leaf.DNSNames, old.leaf.IPAddresses, now)
	if err != nil {
		return nil, err
	}
	certFile, keyFile := filepath.Join(dir, CertFileName), filepath.Join(dir, KeyFileName)
	unlock, err := lockDir(dir, true)
	if err != nil {
		return nil, err
	}
	defer unlock()
	// A regenerate (by another process) may have replaced the key since
	// old was loaded. A certificate for the old key must not go next to
	// the new one.
	if err := keyFileHolds(keyFile, old.leaf); err != nil {
		return nil, err
	}
	if err := writeFile(certFile, encodeCert(der), 0o644); err != nil {
		return nil, fmt.Errorf("apitls: write certificate: %w", err)
	}
	return loadPair(certFile, keyFile)
}
