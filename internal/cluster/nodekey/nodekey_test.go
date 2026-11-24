package nodekey

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testID = "0b5e3a52-7f0e-4c58-9d0b-3f1d6f0a9c11"

func TestCreateLoadKeepsPin(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	k, err := Create(dir, testID, now)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, KeyFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v, mode %v", err, info.Mode().Perm())
	}
	pub, ok := k.Certificate().Leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		t.Fatal("the node key is not ECDSA P-256")
	}
	if got := k.NotAfter().Sub(now); got < Validity-time.Minute || got > Validity+time.Minute {
		t.Fatalf("validity %v", got)
	}
	// A restart reads the same key: the pin is the same.
	again, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if again.Pin() != k.Pin() || len(k.Pin()) != len("sha256:")+64 {
		t.Fatalf("pin changed across a load: %q, %q", k.Pin(), again.Pin())
	}
	// A second Create never replaces the key.
	if _, err := Create(dir, testID, now); err == nil {
		t.Fatal("Create replaced an existing key")
	}
	if third, _ := Load(dir); third.Pin() != k.Pin() {
		t.Fatal("the refused Create changed the key")
	}
}

func TestRenewKeepsKeyAndPin(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	k, err := Create(dir, testID, now)
	if err != nil {
		t.Fatal(err)
	}
	pin, notAfter := k.Pin(), k.NotAfter()
	if renewed, err := k.RenewIfDue(testID, now.Add(Validity-RenewBefore-time.Hour)); err != nil || renewed {
		t.Fatalf("renewed early: %v, %v", renewed, err)
	}
	later := now.Add(Validity - RenewBefore + time.Hour)
	renewed, err := k.RenewIfDue(testID, later)
	if err != nil || !renewed {
		t.Fatalf("not renewed when due: %v, %v", renewed, err)
	}
	if k.Pin() != pin {
		t.Fatal("renewal changed the pin")
	}
	if !k.NotAfter().After(notAfter) {
		t.Fatal("renewal did not extend the certificate")
	}
	loaded, err := Load(dir)
	if err != nil || loaded.Pin() != pin || !loaded.NotAfter().Equal(k.NotAfter()) {
		t.Fatalf("the renewed certificate was not stored: %v", err)
	}
}

func TestLoadRefusals(t *testing.T) {
	dir := t.TempDir()
	if _, err := Create(dir, testID, time.Now()); err != nil {
		t.Fatal(err)
	}
	// A key others can read is refused.
	if err := os.Chmod(filepath.Join(dir, KeyFile), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("a group-readable key was loaded")
	}
	if err := os.Chmod(filepath.Join(dir, KeyFile), 0o600); err != nil {
		t.Fatal(err)
	}
	// A certificate of another key is refused.
	other := t.TempDir()
	if _, err := Create(other, testID, time.Now()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(other, CertFile))
	if err := os.WriteFile(filepath.Join(dir, CertFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("a certificate of another key was accepted")
	}
	if Exists(t.TempDir()) || !Exists(dir) {
		t.Fatal("Exists is wrong")
	}
}
