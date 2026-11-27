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
	// NextKeyFile is the next key while the node rotates its key (step
	// 8b-2b, D1): written before the rotation is asked for, renamed to
	// KeyFile when the node switches.
	NextKeyFile = "node.key.next"
	// Validity is the certificate's lifetime, as skep-api's.
	Validity = 397 * 24 * time.Hour
	// RenewBefore is how long before expiry the certificate is renewed.
	RenewBefore = 30 * 24 * time.Hour
	keyMode     = fs.FileMode(0o600)
	certMode    = fs.FileMode(0o644)
)

// Common-name prefixes of the self-signed certificate: the node ID
// follows. skep-cluster's key is the default; skep-api's node-to-node key
// (step 8e-1, D1) has its own.
const (
	PrefixCluster = "skep-cluster node "
	PrefixAPI     = "skep-api node "
)

// Key is a node key with its current certificate.
type Key struct {
	dir string
	// prefix is the certificate's common-name prefix; "" is
	// PrefixCluster.
	prefix string
	key    *ecdsa.PrivateKey
	pin    string
	mu     sync.RWMutex
	leaf   *x509.Certificate
	tls    *tls.Certificate
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
	return CreatePrefix(dir, PrefixCluster, nodeID, now)
}

// CreatePrefix is Create with the certificate's common-name prefix.
func CreatePrefix(dir, prefix, nodeID string, now time.Time) (*Key, error) {
	priv, err := writeNewKey(dir, KeyFile)
	if err != nil {
		return nil, err
	}
	keyPath := filepath.Join(dir, KeyFile)
	k := &Key{dir: dir, prefix: prefix, key: priv}
	if err := k.issue(nodeID, now); err != nil {
		_ = os.Remove(keyPath)
		return nil, err
	}
	return k, nil
}

// CreateNext generates the next key of a rotation in NextKeyFile (0600,
// fsync, fsync of dir) and returns its pin. It refuses when the file
// exists. No certificate is issued until the node switches to it.
func CreateNext(dir string) (string, error) {
	priv, err := writeNewKey(dir, NextKeyFile)
	if err != nil {
		return "", err
	}
	if err := syncDir(dir); err != nil {
		_ = os.Remove(filepath.Join(dir, NextKeyFile))
		return "", fmt.Errorf("nodekey: %w", err)
	}
	return pinOfPublic(&priv.PublicKey)
}

// FilePin returns the pin of the private key in dir/name, without a
// certificate (the start rule of a rotation reads node.key with it).
func FilePin(dir, name string) (string, error) {
	k, err := loadKeyFile(dir, name)
	if err != nil {
		return "", err
	}
	return pinOfPublic(&k.key.PublicKey)
}

func pinOfPublic(pub *ecdsa.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("nodekey: %w", err)
	}
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

// writeNewKey generates a key and writes it to dir/name with O_EXCL.
func writeNewKey(dir, name string) (*ecdsa.PrivateKey, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("nodekey: generate: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("nodekey: %w", err)
	}
	keyPath := filepath.Join(dir, name)
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
	return priv, nil
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
	return LoadRepairPrefix(dir, PrefixCluster, nodeID, now)
}

// LoadRepairPrefix is LoadRepair with the certificate's common-name
// prefix for a certificate it issues.
func LoadRepairPrefix(dir, prefix, nodeID string, now time.Time) (k *Key, repaired error, err error) {
	if k, err = loadKey(dir); err != nil {
		return nil, nil, err
	}
	k.prefix = prefix
	if repaired = k.loadCertificate(); repaired == nil {
		return k, nil, nil
	}
	if err := k.issue(nodeID, now); err != nil {
		return nil, nil, fmt.Errorf("%w (and issuing a new certificate failed: %w)", repaired, err)
	}
	return k, repaired, nil
}

func loadKey(dir string) (*Key, error) { return loadKeyFile(dir, KeyFile) }

func loadKeyFile(dir, name string) (*Key, error) {
	keyPath := filepath.Join(dir, name)
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
	if !ok || !pub.Equal(&k.priv().PublicKey) {
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

func (k *Key) priv() *ecdsa.PrivateKey {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.key
}

// Adopt makes k the key o holds, in place, so that everything that holds
// k (the listener, the diallers, the transport) uses the new key from its
// next handshake on (the switch of a key rotation, D1 of step 8b-2b).
func (k *Key) Adopt(o *Key) {
	o.mu.RLock()
	key, leaf, pin, tc := o.key, o.leaf, o.pin, o.tls
	o.mu.RUnlock()
	k.mu.Lock()
	defer k.mu.Unlock()
	k.key, k.leaf, k.pin, k.tls = key, leaf, pin, tc
}

func (k *Key) commonNamePrefix() string {
	if k.prefix == "" {
		return PrefixCluster
	}
	return k.prefix
}

// Sign signs digest (a SHA-256 hash) with the key: an ASN.1 ECDSA
// signature. The private key never leaves the package.
func (k *Key) Sign(digest []byte) ([]byte, error) {
	return ecdsa.SignASN1(rand.Reader, k.priv(), digest)
}

// PublicKeyDER returns the key's SubjectPublicKeyInfo (DER), whose
// SHA-256 is the pin.
func (k *Key) PublicKeyDER() ([]byte, error) {
	return x509.MarshalPKIXPublicKey(&k.priv().PublicKey)
}

// issue self-signs a certificate for the key and stores it.
func (k *Key) issue(nodeID string, now time.Time) error {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return fmt.Errorf("nodekey: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: k.commonNamePrefix() + nodeID},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(Validity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	priv := k.priv()
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
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
