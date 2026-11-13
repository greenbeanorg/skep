package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/greenbeanorg/skep/internal/apidb"
	"github.com/greenbeanorg/skep/internal/apitls"
	"github.com/greenbeanorg/skep/internal/audit"
	"github.com/greenbeanorg/skep/internal/ids"
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
	// Open the audit log first: a key change whose intent cannot be
	// recorded does not happen.
	db, err := openDatabase(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer db.Close()

	// Two entries, like the task engine's requested and finished pair,
	// joined by operation_id. The access log has no phase column, so the
	// phase is in the details. The intent is written after the new pair
	// exists in memory and before anything on disk changes: a key change
	// that completes after a crash (skep-api finishes a half-done swap at
	// its next start) is still on record, with the key that was
	// installed. If the intent cannot be written, nothing changes.
	opID, err := ids.NewUUID()
	if err != nil {
		return err
	}
	access := apidb.NewAccessLog(db)
	actor := audit.LocalActor(e.uid)
	record := func(phase audit.Phase, outcome audit.Outcome, details map[string]any) error {
		details["phase"], details["operation_id"] = string(phase), opID
		return access.Write(ctx, audit.AccessEntry{
			ActorName: actor, Action: apitls.ActionRegenerate, ResourceType: "tls",
			Outcome: outcome, Details: details,
		})
	}
	pins := func(before, next apitls.Info) map[string]any {
		d := map[string]any{
			"new_public_key_sha256":  next.PublicKeySHA256,
			"new_certificate_sha256": next.CertificateSHA256,
			"not_after":              next.NotAfter.UTC().Format(time.RFC3339),
		}
		if before.PublicKeySHA256 != "" {
			d["old_public_key_sha256"] = before.PublicKeySHA256
			d["old_certificate_sha256"] = before.CertificateSHA256
		}
		return d
	}

	var next apitls.Info
	before, after, err := apitls.Regenerate(cfg.TLSDir(), time.Now(), func(before, n apitls.Info) error {
		next = n
		if err := record(audit.Requested, audit.Success, pins(before, n)); err != nil {
			return fmt.Errorf("the key change could not be recorded, so nothing was changed: %w", err)
		}
		return nil
	})
	if err != nil {
		if next.PublicKeySHA256 == "" {
			// The intent was not written (or the pair could not be
			// made): nothing on disk changed, so there is no outcome.
			return err
		}
		details := pins(before, next)
		details["error"] = err.Error()
		if werr := record(audit.Finished, audit.Failure, details); werr != nil {
			log.Error("could not write the outcome of the key change to the access audit log", "error", werr)
		}
		return err
	}
	if err := record(audit.Finished, audit.Success, pins(before, after)); err != nil {
		return fmt.Errorf("the key was replaced and its intent is recorded, but the outcome could not be written: %w", err)
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
