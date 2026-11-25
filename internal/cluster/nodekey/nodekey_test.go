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

// At the service's start a good key with a bad certificate gets a new
// certificate with the same key, so the pin does not change.
func TestLoadRepairIssuesANewCertificate(t *testing.T) {
	other := t.TempDir()
	if _, err := Create(other, testID, time.Now()); err != nil {
		t.Fatal(err)
	}
	otherCert, err := os.ReadFile(filepath.Join(other, CertFile))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(t *testing.T, cert string){
		"missing": func(t *testing.T, cert string) {
			if err := os.Remove(cert); err != nil {
				t.Fatal(err)
			}
		},
		"empty": func(t *testing.T, cert string) {
			if err := os.WriteFile(cert, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"unreadable": func(t *testing.T, cert string) {
			if os.Geteuid() == 0 {
				t.Skip("root reads a mode 0000 file")
			}
			if err := os.Chmod(cert, 0); err != nil {
				t.Fatal(err)
			}
		},
		"another key's": func(t *testing.T, cert string) {
			if err := os.WriteFile(cert, otherCert, 0o644); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, spoil := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			k, err := Create(dir, testID, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			cert := filepath.Join(dir, CertFile)
			spoil(t, cert)
			if _, err := Load(dir); err == nil {
				t.Fatal("Load accepted the spoiled certificate")
			}
			got, repaired, err := LoadRepair(dir, testID, time.Now())
			if err != nil || repaired == nil {
				t.Fatalf("LoadRepair: repaired %v, err %v", repaired, err)
			}
			if got.Pin() != k.Pin() {
				t.Fatal("the repair changed the pin")
			}
			info, err := os.Stat(cert)
			if err != nil || info.Mode().Perm() != certMode {
				t.Fatalf("new certificate: %v, mode %v", err, info.Mode().Perm())
			}
			again, err := Load(dir)
			if err != nil || again.Pin() != k.Pin() {
				t.Fatalf("the new certificate does not load: %v", err)
			}
			if matches, _ := filepath.Glob(cert + ".tmp-*"); len(matches) != 0 {
				t.Fatalf("temporary files left: %v", matches)
			}
		})
	}
	// A good pair is loaded as it is.
	dir := t.TempDir()
	k, err := Create(dir, testID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	got, repaired, err := LoadRepair(dir, testID, time.Now())
	if err != nil || repaired != nil || !got.NotAfter().Equal(k.NotAfter()) {
		t.Fatalf("a good pair: repaired %v, err %v", repaired, err)
	}
	// A bad key is never repaired.
	if err := os.WriteFile(filepath.Join(dir, KeyFile), []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadRepair(dir, testID, time.Now()); err == nil {
		t.Fatal("LoadRepair accepted a bad key")
	}
}

// Step 8b-2b: the next key of a rotation, its pin from the key file alone
// (the same as the certificate's), and the in-place switch.
func TestNextKeyAndAdopt(t *testing.T) {
	dir := t.TempDir()
	k, err := Create(dir, testID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if p, err := FilePin(dir, KeyFile); err != nil || p != k.Pin() {
		t.Fatalf("file pin %s %v, certificate pin %s", p, err, k.Pin())
	}
	next, err := CreateNext(dir)
	if err != nil || next == k.Pin() {
		t.Fatalf("next %s %v", next, err)
	}
	if _, err := CreateNext(dir); err == nil {
		t.Fatal("a second next key")
	}
	info, _ := os.Stat(filepath.Join(dir, NextKeyFile))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
	if p, _ := FilePin(dir, NextKeyFile); p != next {
		t.Fatalf("next pin %s", p)
	}
	held := k.Certificate
	if err := os.Rename(filepath.Join(dir, NextKeyFile), filepath.Join(dir, KeyFile)); err != nil {
		t.Fatal(err)
	}
	nk, repaired, err := LoadRepair(dir, testID, time.Now())
	if err != nil || repaired == nil || nk.Pin() != next {
		t.Fatalf("after the rename: %v %v %s", repaired, err, nk.Pin())
	}
	k.Adopt(nk)
	if k.Pin() != next || PinOfCertificate(held().Leaf) != next {
		t.Fatal("the adopted key is not used through the held certificate function")
	}
}
