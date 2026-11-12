package auth

import (
	"encoding/hex"
	"strings"
	"testing"
)

// Known answers from the test suite of the Argon2 reference implementation
// (phc-winner-argon2, src/test.c, Argon2id, version 0x13). The RFC 9106
// test vector cannot be used: it sets the optional secret and associated
// data inputs, which golang.org/x/crypto/argon2 does not expose and Skep
// does not use.
var referenceVectors = []struct {
	passes, memoryKiB uint32
	lanes             uint8
	password, salt    string
	hash, encoded     string
}{
	{2, 1 << 16, 1, "password", "somesalt",
		"09316115d5cf24ed5a15a31a3ba326e5cf32edc24702987c02b6566f61913cf7",
		"$argon2id$v=19$m=65536,t=2,p=1$c29tZXNhbHQ$CTFhFdXPJO1aFaMaO6Mm5c8y7cJHAph8ArZWb2GRPPc"},
	{2, 1 << 18, 1, "password", "somesalt",
		"78fe1ec91fb3aa5657d72e710854e4c3d9b9198c742f9616c2f085bed95b2e8c",
		"$argon2id$v=19$m=262144,t=2,p=1$c29tZXNhbHQ$eP4eyR+zqlZX1y5xCFTkw9m5GYx0L5YWwvCFvtlbLow"},
	{2, 1 << 8, 1, "password", "somesalt",
		"9dfeb910e80bad0311fee20f9c0e2b12c17987b4cac90c2ef54d5b3021c68bfe",
		"$argon2id$v=19$m=256,t=2,p=1$c29tZXNhbHQ$nf65EOgLrQMR/uIPnA4rEsF5h7TKyQwu9U1bMCHGi/4"},
	{2, 1 << 8, 2, "password", "somesalt",
		"6d093c501fd5999645e0ea3bf620d7b8be7fd2db59c20d9fff9539da2bf57037",
		"$argon2id$v=19$m=256,t=2,p=2$c29tZXNhbHQ$bQk8UB/VmZZF4Oo79iDXuL5/0ttZwg2f/5U52iv1cDc"},
	{1, 1 << 16, 1, "password", "somesalt",
		"f6a5adc1ba723dddef9b5ac1d464e180fcd9dffc9d1cbf76cca2fed795d9ca98",
		"$argon2id$v=19$m=65536,t=1,p=1$c29tZXNhbHQ$9qWtwbpyPd3vm1rB1GThgPzZ3/ydHL92zKL+15XZypg"},
	{4, 1 << 16, 1, "password", "somesalt",
		"9025d48e68ef7395cca9079da4c4ec3affb3c8911fe4f86d1a2520856f63172c",
		"$argon2id$v=19$m=65536,t=4,p=1$c29tZXNhbHQ$kCXUjmjvc5XMqQedpMTsOv+zyJEf5PhtGiUghW9jFyw"},
	{2, 1 << 16, 1, "differentpassword", "somesalt",
		"0b84d652cf6b0c4beaef0dfe278ba6a80df6696281d7e0d2891b817d8c458fde",
		"$argon2id$v=19$m=65536,t=2,p=1$c29tZXNhbHQ$C4TWUs9rDEvq7w3+J4umqA32aWKB1+DSiRuBfYxFj94"},
	{2, 1 << 16, 1, "password", "diffsalt",
		"bdf32b05ccc42eb15d58fd19b1f856b113da1e9a5874fdcc544308565aa8141c",
		"$argon2id$v=19$m=65536,t=2,p=1$ZGlmZnNhbHQ$vfMrBczELrFdWP0ZsfhWsRPaHppYdP3MVEMIVlqoFBw"},
}

func TestArgon2idReferenceVectors(t *testing.T) {
	for _, v := range referenceVectors {
		p := Params{MemoryKiB: v.memoryKiB, Passes: v.passes, Lanes: v.lanes}
		got := derive(v.password, []byte(v.salt), p, hashLength)
		if hex.EncodeToString(got) != v.hash {
			t.Errorf("m=%d t=%d p=%d %q/%q: hash = %x, want %s", v.memoryKiB, v.passes, v.lanes, v.password, v.salt, got, v.hash)
		}
		if enc := encodePHC(p, []byte(v.salt), got); enc != v.encoded {
			t.Errorf("encoded = %s, want %s", enc, v.encoded)
		}
		// The reference's encoded strings verify, and only with the right
		// password.
		ok, used, err := verifyPassword(v.password, v.encoded)
		if err != nil || !ok || used != p {
			t.Errorf("verify %s: ok=%v params=%+v err=%v", v.encoded, ok, used, err)
		}
		if ok, _, _ := verifyPassword(v.password+"x", v.encoded); ok {
			t.Errorf("a wrong password verified against %s", v.encoded)
		}
	}
}

func TestHashPasswordRoundTrip(t *testing.T) {
	p := MinParams
	a, err := hashPassword("correct horse battery staple", p)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := hashPassword("correct horse battery staple", p)
	if a == b {
		t.Error("two hashes of one password are identical: the salt is not random")
	}
	if !strings.HasPrefix(a, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Errorf("hash = %s", a)
	}
	gotP, salt, hash, err := parsePHC(a)
	if err != nil || gotP != p || len(salt) != 16 || len(hash) != 32 {
		t.Errorf("parse: %+v, salt %d, hash %d, %v", gotP, len(salt), len(hash), err)
	}
	if ok, _, err := verifyPassword("correct horse battery staple", a); !ok || err != nil {
		t.Errorf("the right password did not verify: %v", err)
	}
	if ok, _, _ := verifyPassword("correct horse battery stapl", a); ok {
		t.Error("a wrong password verified")
	}
}

func TestParsePHCIsStrict(t *testing.T) {
	good := "$argon2id$v=19$m=65536,t=2,p=1$c29tZXNhbHQ$CTFhFdXPJO1aFaMaO6Mm5c8y7cJHAph8ArZWb2GRPPc"
	if _, _, _, err := parsePHC(good); err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]string{
		"empty":            "",
		"argon2i":          strings.Replace(good, "argon2id", "argon2i", 1),
		"old version":      strings.Replace(good, "v=19", "v=16", 1),
		"no version":       strings.Replace(good, "v=19$", "", 1),
		"reordered params": strings.Replace(good, "m=65536,t=2,p=1", "t=2,m=65536,p=1", 1),
		"missing param":    strings.Replace(good, ",p=1", "", 1),
		"extra param":      strings.Replace(good, "p=1", "p=1,keyid=x", 1),
		"zero passes":      strings.Replace(good, "t=2", "t=0", 1),
		"leading zero":     strings.Replace(good, "t=2", "t=02", 1),
		"plus sign":        strings.Replace(good, "t=2", "t=+2", 1),
		"negative":         strings.Replace(good, "t=2", "t=-2", 1),
		"huge memory":      strings.Replace(good, "m=65536", "m=4294967295", 1),
		"huge passes":      strings.Replace(good, "t=2", "t=1000000", 1),
		"many lanes":       strings.Replace(good, "p=1", "p=200", 1),
		"tiny memory":      strings.Replace(good, "m=65536", "m=4", 1),
		"padded salt":      strings.Replace(good, "c29tZXNhbHQ", "c29tZXNhbHQ=", 1),
		"bad salt":         strings.Replace(good, "c29tZXNhbHQ", "c29t!XNhbHQ", 1),
		"short salt":       strings.Replace(good, "c29tZXNhbHQ", "c29t", 1),
		"short hash":       good[:len(good)-30],
		"extra field":      good + "$extra",
		"trailing newline": good + "\n",
	} {
		if _, _, _, err := parsePHC(s); err == nil {
			t.Errorf("%s: accepted %q", name, s)
		}
		if ok, _, err := verifyPassword("password", s); ok || err == nil {
			t.Errorf("%s: verifyPassword = %v, %v", name, ok, err)
		}
	}
}

func TestParamsValidate(t *testing.T) {
	for _, p := range []Params{DefaultParams, MinParams, {MemoryKiB: 256 * 1024, Passes: 4, Lanes: 2}} {
		if err := p.Validate(); err != nil {
			t.Errorf("%+v: %v", p, err)
		}
	}
	for name, p := range map[string]Params{
		"below minimum memory": {MemoryKiB: 19455, Passes: 2, Lanes: 1},
		"one pass":             {MemoryKiB: 65536, Passes: 1, Lanes: 1},
		"no lanes":             {MemoryKiB: 65536, Passes: 3, Lanes: 0},
		"too much memory":      {MemoryKiB: 2 * 1024 * 1024, Passes: 3, Lanes: 1},
		"too many passes":      {MemoryKiB: 65536, Passes: 17, Lanes: 1},
		"zero value":           {},
	} {
		if err := p.Validate(); err == nil {
			t.Errorf("%s: %+v accepted", name, p)
		}
	}
	if DefaultParams != (Params{MemoryKiB: 65536, Passes: 3, Lanes: 1}) {
		t.Errorf("default parameters = %+v, want 64 MiB, 3 passes, 1 lane (ADR 0006)", DefaultParams)
	}
	weak, strong := Params{65536, 2, 1}, Params{65536, 3, 1}
	if !weak.weakerThan(strong) || strong.weakerThan(weak) || strong.weakerThan(strong) {
		t.Error("weakerThan is wrong")
	}
}
