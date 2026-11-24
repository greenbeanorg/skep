// Package nodekey holds a node's cluster identity: an ECDSA P-256 key and
// a self-signed certificate (ADR 0013 B1). Other nodes trust the key by
// its pin, the SHA-256 of the public key (SubjectPublicKeyInfo), never by
// the certificate's signature or names. The certificate is renewed with
// the same key, so the pin does not change.
package nodekey

import (
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
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

const (
	// KeyFile and CertFile are the file names in the data directory.
	KeyFile  = "node.key"
	CertFile = "node.crt"
	// Validity is the certificate's lifetime, as skep-api's.
	Validity = 397 * 24 * time.Hour
	// RenewBefore is how long before expiry the certificate is renewed.
	RenewBefore = 30 * 24 * time.Hour
	keyMode     = fs.FileMode(0o600)
	certMode    = fs.FileMode(0o644)
)

// Key is a node key with its current certificate.
type Key struct {
	dir  string
	key  *ecdsa.PrivateKey
	pin  string
	mu   sync.RWMutex
	leaf *x509.Certificate
	tls  *tls.Certificate
}

// PinOfCertificate returns the pin of a certificate's public key:
// "sha256:" and the hex SHA-256 of its SubjectPublicKeyInfo, the format
// skepctl uses for skep-api's key.
func PinOfCertificate(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Exists reports whether dir holds a node key.
func Exists(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, KeyFile))
	return err == nil
}

// Create generates a new key and certificate in dir. It refuses when a key
// is already there: a node's key is replaced only by an explicit rotation
// or a wipe, never by accident.
func Create(dir, nodeID string, now time.Time) (*Key, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("nodekey: generate: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("nodekey: %w", err)
	}
	keyPath := filepath.Join(dir, KeyFile)
	f, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, keyMode)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("nodekey: %s already exists; a node key is never replaced by init", keyPath)
		}
		return nil, fmt.Errorf("nodekey: %w", err)
	}
	werr := pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if werr == nil {
		werr = f.Chmod(keyMode)
	}
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(keyPath)
		return nil, fmt.Errorf("nodekey: write %s: %w", keyPath, werr)
	}
	k := &Key{dir: dir, key: priv}
	if err := k.issue(nodeID, now); err != nil {
		_ = os.Remove(keyPath)
		return nil, err
	}
	return k, nil
}

// Load reads the key and certificate from dir. It refuses a key file that
// anyone but its owner can read, and a certificate that does not belong to
// the key.
func Load(dir string) (*Key, error) {
	k, err := loadKey(dir)
	if err != nil {
		return nil, err
	}
	if err := k.loadCertificate(); err != nil {
		return nil, err
	}
	return k, nil
}

// LoadRepair is Load for the service's start: when the key is good but the
// certificate is missing, empty, unreadable or not for that key, it issues
// a new certificate with the same key, so the pin does not change. repaired
// is then why; the caller logs it as a warning. A bad key is still an
// error: a node key is never replaced here.
func LoadRepair(dir, nodeID string, now time.Time) (k *Key, repaired error, err error) {
	if k, err = loadKey(dir); err != nil {
		return nil, nil, err
	}
	if repaired = k.loadCertificate(); repaired == nil {
		return k, nil, nil
	}
	if err := k.issue(nodeID, now); err != nil {
		return nil, nil, fmt.Errorf("%w (and issuing a new certificate failed: %w)", repaired, err)
	}
	return k, repaired, nil
}

func loadKey(dir string) (*Key, error) {
	keyPath := filepath.Join(dir, KeyFile)
	info, err := os.Lstat(keyPath)
	if err != nil {
		return nil, fmt.Errorf("nodekey: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("nodekey: %s is not a regular file", keyPath)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("nodekey: %s has mode %04o; it must be 0600", keyPath, info.Mode().Perm())
	}
	b, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("nodekey: %w", err)
	}
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("nodekey: %s holds no private key", keyPath)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("nodekey: %s: %w", keyPath, err)
	}
	priv, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || priv.Curve != elliptic.P256() {
		return nil, fmt.Errorf("nodekey: %s is not an ECDSA P-256 key", keyPath)
	}
	return &Key{dir: dir, key: priv}, nil
}

func (k *Key) loadCertificate() error {
	certPath := filepath.Join(k.dir, CertFile)
	cb, err := os.ReadFile(certPath)
	if err != nil {
		return fmt.Errorf("nodekey: %w", err)
	}
	cblock, _ := pem.Decode(cb)
	if cblock == nil || cblock.Type != "CERTIFICATE" {
		return fmt.Errorf("nodekey: %s holds no certificate", certPath)
	}
	leaf, err := x509.ParseCertificate(cblock.Bytes)
	if err != nil {
		return fmt.Errorf("nodekey: %s: %w", certPath, err)
	}
	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&k.key.PublicKey) {
		return fmt.Errorf("nodekey: %s does not belong to %s", certPath, filepath.Join(k.dir, KeyFile))
	}
	k.set(leaf)
	return nil
}

func (k *Key) set(leaf *x509.Certificate) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.leaf = leaf
	k.pin = PinOfCertificate(leaf)
	k.tls = &tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: k.key, Leaf: leaf}
}

// issue self-signs a certificate for the key and stores it.
func (k *Key) issue(nodeID string, now time.Time) error {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return fmt.Errorf("nodekey: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "skep-cluster node " + nodeID},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(Validity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.key.PublicKey, k.key)
	if err != nil {
		return fmt.Errorf("nodekey: sign: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return fmt.Errorf("nodekey: %w", err)
	}
	certPath := filepath.Join(k.dir, CertFile)
	if err := writeFileSynced(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), certMode); err != nil {
		return fmt.Errorf("nodekey: %w", err)
	}
	k.set(leaf)
	return nil
}

// writeFileSynced replaces path atomically: a temporary file in the same
// directory, fsync, rename, fsync of the directory.
func writeFileSynced(path string, data []byte, mode fs.FileMode) error {
	tmp := path + ".tmp-" + strconv.Itoa(os.Getpid())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

// Pin returns the pin of the node key.
func (k *Key) Pin() string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.pin
}

// NotAfter returns when the current certificate expires.
func (k *Key) NotAfter() time.Time {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.leaf.NotAfter
}

// Certificate returns the current certificate for a TLS handshake.
func (k *Key) Certificate() *tls.Certificate {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.tls
}

// RenewIfDue issues a new certificate with the same key when the current
// one expires within RenewBefore. It reports whether it did.
func (k *Key) RenewIfDue(nodeID string, now time.Time) (bool, error) {
	if k.NotAfter().Sub(now) > RenewBefore {
		return false, nil
	}
	if err := k.issue(nodeID, now); err != nil {
		return false, err
	}
	return true, nil
}
