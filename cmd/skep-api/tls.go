package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/greenbeanorg/skep/internal/apidb"
	"io"
	"strings"
	"time"

	"github.com/greenbeanorg/skep/internal/apitls"
	"github.com/greenbeanorg/skep/internal/audit"
)

const tlsUsage = `usage:
  skep-api tls fingerprint [flags]   print the public key pin of the certificate in use
  skep-api tls regenerate  [flags]   replace the generated key and certificate with new ones`

// runTLS dispatches the "tls" commands.
func runTLS(ctx context.Context, args []string, e env) error {
	if len(args) == 0 {
		fmt.Fprintln(e.stderr, tlsUsage)
		return errors.New("missing tls command")
	}
	switch args[0] {
	case "fingerprint":
		return tlsFingerprint(args[1:], e)
	case "regenerate":
		return tlsRegenerate(ctx, args[1:], e)
	default:
		fmt.Fprintln(e.stderr, tlsUsage)
		return fmt.Errorf("unknown tls command %q", args[0])
	}
}

func printCertificate(w io.Writer, info apitls.Info) {
	fmt.Fprintf(w, "source:       %s\n", info.Source)
	fmt.Fprintf(w, "public key:   %s\n", info.PublicKeySHA256)
	fmt.Fprintf(w, "certificate:  %s\n", info.CertificateSHA256)
	fmt.Fprintf(w, "valid until:  %s\n", info.NotAfter.UTC().Format(time.RFC3339))
	fmt.Fprintf(w, "names:        %s\n", strings.Join(append(append([]string(nil), info.DNSNames...), info.IPAddresses...), ", "))
}

// tlsFingerprint prints the SHA-256 of the certificate's public key, the
// value clients pin, so it can be compared out of band.
func tlsFingerprint(args []string, e env) error {
	flags := newLocalFlags("skep-api tls fingerprint", e.stderr)
	cfg, err := flags.prepare(args, e)
	if err != nil {
		return err
	}
	info, err := apitls.Fingerprint(cfg.TLSDir(), cfg.TLS.CertFile, cfg.TLS.KeyFile)
	if err != nil {
		return err
	}
	printCertificate(e.stdout, info)
	return nil
}

// tlsRegenerate creates a new key and a new self-signed certificate. It is
// the only way the key ever changes; every client that pinned the old key
// has to confirm the new one.
func tlsRegenerate(ctx context.Context, args []string, e env) error {
	flags := newLocalFlags("skep-api tls regenerate", e.stderr)
	cfg, err := flags.prepare(args, e)
	if err != nil {
		return err
	}
	if cfg.TLS.CertFile != "" {
		return fmt.Errorf("an operator-supplied certificate is configured (%s); Skep never replaces operator files", cfg.TLS.CertFile)
	}
	log := quietLogger(e.stderr)
	// Open the audit log first: a key change that cannot be recorded
	// does not happen.
	db, err := openDatabase(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer db.Close()

	before, after, err := apitls.Regenerate(cfg.TLSDir(), time.Now())
	if err != nil {
		return err
	}
	details := map[string]any{
		"new_public_key_sha256":  after.PublicKeySHA256,
		"new_certificate_sha256": after.CertificateSHA256,
		"not_after":              after.NotAfter.UTC().Format(time.RFC3339),
	}
	if before.PublicKeySHA256 != "" {
		details["old_public_key_sha256"] = before.PublicKeySHA256
		details["old_certificate_sha256"] = before.CertificateSHA256
	}
	if err := apidb.NewAccessLog(db).Write(ctx, audit.AccessEntry{
		ActorName: audit.LocalActor(e.uid), Action: apitls.ActionRegenerate, ResourceType: "tls",
		Outcome: audit.Success, Details: details,
	}); err != nil {
		return fmt.Errorf("the key was replaced, but the audit entry could not be written: %w", err)
	}

	if before.PublicKeySHA256 != "" {
		fmt.Fprintf(e.stdout, "old public key: %s\n", before.PublicKeySHA256)
	}
	fmt.Fprintln(e.stdout, "A new key and certificate were generated.")
	printCertificate(e.stdout, after)
	fmt.Fprintln(e.stdout, "A running skep-api picks them up within a minute (at once with SIGHUP).")
	fmt.Fprintln(e.stdout, "Every client that pinned the old key must confirm the new one.")
	return nil
}
