package apitls

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"
)

// Event is something the manager did that belongs in the access audit log.
type Event struct {
	// Action is "tls.certificate.renew" or "tls.key.regenerate".
	Action string
	// Details holds the old and new fingerprints and the new expiry.
	Details map[string]any
}

// Actions.
const (
	ActionRenew      = "tls.certificate.renew"
	ActionRegenerate = "tls.key.regenerate"
)

// Options configures a Manager.
type Options struct {
	// Dir holds the generated key and certificate.
	Dir string
	// CertFile and KeyFile name an operator-supplied pair. Set both or
	// neither. When set, Dir is not used and nothing is ever generated.
	CertFile string
	KeyFile  string
	// Audit records renewals. Optional.
	Audit func(ctx context.Context, e Event)
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Now defaults to time.Now. Tests replace it.
	Now func() time.Time
}

// Manager holds the certificate skep-api serves and keeps it current.
type Manager struct {
	opts   Options
	source Source
	log    *slog.Logger
	now    func() time.Time

	mu         sync.RWMutex
	current    *loaded
	lastWarned time.Time
}

// New loads the certificate, generating or renewing it first when the
// source is self-signed and that is needed.
func New(ctx context.Context, opts Options) (*Manager, error) {
	if (opts.CertFile == "") != (opts.KeyFile == "") {
		return nil, errors.New("apitls: tls.cert_file and tls.key_file must be set together")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	m := &Manager{opts: opts, source: SelfSigned, log: opts.Logger, now: opts.Now}

	if opts.CertFile != "" {
		m.source = Operator
		l, err := loadPair(opts.CertFile, opts.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("apitls: operator-supplied certificate: %w", err)
		}
		m.current = l
		m.log.Info("TLS: using the operator-supplied certificate",
			"cert_file", opts.CertFile, "not_after", l.leaf.NotAfter, "public_key", fingerprint(l.leaf.RawSubjectPublicKeyInfo))
		m.checkExpiry(ctx)
		return m, nil
	}

	if opts.Dir == "" {
		return nil, errors.New("apitls: the TLS directory must not be empty")
	}
	l, err := loadGenerated(opts.Dir)
	switch {
	case errors.Is(err, errNoCertificate):
		if l, err = generate(opts.Dir, m.now()); err != nil {
			return nil, err
		}
		m.log.Info("TLS: generated a self-signed certificate and a new key",
			"dir", opts.Dir, "not_after", l.leaf.NotAfter, "names", l.leaf.DNSNames,
			"public_key", fingerprint(l.leaf.RawSubjectPublicKeyInfo))
	case err != nil:
		// Never replace a key silently: clients may have pinned it.
		return nil, fmt.Errorf("%w; fix the files or run 'skep-api tls regenerate' to create a new key", err)
	default:
		m.log.Info("TLS: using the generated self-signed certificate",
			"dir", opts.Dir, "not_after", l.leaf.NotAfter, "public_key", fingerprint(l.leaf.RawSubjectPublicKeyInfo))
	}
	m.current = l
	m.checkExpiry(ctx)
	return m, nil
}

// Source says whether the certificate is generated or operator-supplied.
func (m *Manager) Source() Source { return m.source }

// Info describes the certificate in use.
func (m *Manager) Info() Info {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return infoFor(m.source, m.current.leaf)
}

// GetCertificate is the tls.Config hook. Every handshake gets the current
// certificate, so a renewal or reload needs no restart and drops no
// connection.
func (m *Manager) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current.cert, nil
}

// TLSConfig returns the server configuration: TLS 1.3 only.
func (m *Manager) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS13,
		GetCertificate: m.GetCertificate,
		NextProtos:     []string{"h2", "http/1.1"},
	}
}

func (m *Manager) files() (certFile, keyFile string) {
	if m.source == Operator {
		return m.opts.CertFile, m.opts.KeyFile
	}
	return filepath.Join(m.opts.Dir, CertFileName), filepath.Join(m.opts.Dir, KeyFileName)
}

// Reload reads the certificate and key from disk again and then checks the
// expiry. A pair that cannot be loaded, or does not match, is logged and
// the certificate in use stays. It is what SIGHUP does.
func (m *Manager) Reload(ctx context.Context) error {
	err := m.reload(true)
	m.checkExpiry(ctx)
	return err
}

// reload loads the pair from disk when force is set or the files changed.
func (m *Manager) reload(force bool) error {
	certFile, keyFile := m.files()
	m.mu.RLock()
	stamp := m.current.stamp
	m.mu.RUnlock()
	if !force {
		if now := stampOf(certFile, keyFile); now == "" || now == stamp {
			return nil
		}
	}
	var l *loaded
	var err error
	if m.source == Operator {
		l, err = loadPair(certFile, keyFile)
	} else {
		// Finishes a swap that generate has half done, so a reload that
		// lands between its renames gets the new pair.
		l, err = loadGenerated(m.opts.Dir)
	}
	if err != nil {
		m.log.Error("TLS: the certificate files could not be loaded; the certificate in use stays",
			"cert_file", certFile, "error", err)
		return fmt.Errorf("apitls: reload: %w", err)
	}
	m.mu.Lock()
	old := m.current
	m.current = l
	m.mu.Unlock()
	if l.stamp != old.stamp {
		m.log.Info("TLS: certificate reloaded from disk",
			"source", string(m.source), "not_after", l.leaf.NotAfter,
			"public_key", fingerprint(l.leaf.RawSubjectPublicKeyInfo),
			"key_changed", !samePublicKey(old.leaf, l.leaf))
	}
	return nil
}

// checkExpiry renews a generated certificate that is close to expiry, and
// warns about an operator-supplied one, which Skep never rotates.
func (m *Manager) checkExpiry(ctx context.Context) {
	m.mu.RLock()
	cur := m.current
	m.mu.RUnlock()
	now := m.now()
	if now.Before(cur.leaf.NotAfter.Add(-RenewBefore)) {
		return
	}

	if m.source == Operator {
		m.mu.Lock()
		warn := now.Sub(m.lastWarned) >= 24*time.Hour
		if warn {
			m.lastWarned = now
		}
		m.mu.Unlock()
		if warn {
			m.log.Warn("TLS: the operator-supplied certificate expires soon or has expired; Skep does not renew it",
				"cert_file", m.opts.CertFile, "not_after", cur.leaf.NotAfter)
		}
		return
	}

	renewed, err := renew(m.opts.Dir, cur, now)
	if err != nil {
		m.log.Error("TLS: could not renew the self-signed certificate", "error", err, "not_after", cur.leaf.NotAfter)
		return
	}
	m.mu.Lock()
	m.current = renewed
	m.mu.Unlock()
	m.log.Info("TLS: renewed the self-signed certificate with the same key",
		"old_not_after", cur.leaf.NotAfter, "not_after", renewed.leaf.NotAfter,
		"public_key", fingerprint(renewed.leaf.RawSubjectPublicKeyInfo))
	if m.opts.Audit != nil {
		m.opts.Audit(ctx, Event{Action: ActionRenew, Details: map[string]any{
			"old_certificate_sha256": fingerprint(cur.leaf.Raw),
			"new_certificate_sha256": fingerprint(renewed.leaf.Raw),
			"public_key_sha256":      fingerprint(renewed.leaf.RawSubjectPublicKeyInfo),
			"old_not_after":          cur.leaf.NotAfter.UTC().Format(time.RFC3339),
			"not_after":              renewed.leaf.NotAfter.UTC().Format(time.RFC3339),
		}})
	}
}

// Tick is one round of upkeep: pick up files that changed on disk, then
// check the expiry.
func (m *Manager) Tick(ctx context.Context) {
	_ = m.reload(false)
	m.checkExpiry(ctx)
}

// Run does the upkeep until ctx is done: once per interval, and at once
// whenever something arrives on hup.
func (m *Manager) Run(ctx context.Context, interval time.Duration, hup <-chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.Tick(ctx)
		case <-hup:
			m.log.Info("TLS: SIGHUP received, reloading the certificate")
			_ = m.Reload(ctx)
		}
	}
}

// Fingerprint describes the generated certificate in dir, or the
// operator-supplied one, without serving it.
func Fingerprint(dir, certFile, keyFile string) (Info, error) {
	if certFile != "" {
		l, err := loadPair(certFile, keyFile)
		if err != nil {
			return Info{}, fmt.Errorf("apitls: operator-supplied certificate: %w", err)
		}
		return infoFor(Operator, l.leaf), nil
	}
	l, err := loadGenerated(dir)
	if err != nil {
		return Info{}, err
	}
	return infoFor(SelfSigned, l.leaf), nil
}

// Regenerate replaces the generated key and certificate in dir with new
// ones, for the host's current names and addresses. It returns what was
// there before (the zero Info when nothing was) and what is there now. A
// running skep-api picks the new pair up by itself.
func Regenerate(dir string, now time.Time) (before, after Info, err error) {
	if old, loadErr := loadGenerated(dir); loadErr == nil {
		before = infoFor(SelfSigned, old.leaf)
	}
	l, err := generate(dir, now)
	if err != nil {
		return Info{}, Info{}, err
	}
	return before, infoFor(SelfSigned, l.leaf), nil
}
