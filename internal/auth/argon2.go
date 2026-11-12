// Package auth is skep-api's identity logic: password hashing, users and
// roles, sessions, login throttling, and permissions (ADR 0006). It knows
// nothing about HTTP; internal/api translates. Storage is behind small
// interfaces that internal/database implements.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Params are the Argon2id cost parameters. They are stored in every hash,
// so they can be raised later without invalidating existing passwords.
type Params struct {
	// MemoryKiB is the memory cost in KiB.
	MemoryKiB uint32
	// Passes is the number of passes over the memory.
	Passes uint32
	// Lanes is the degree of parallelism.
	Lanes uint8
}

var (
	// DefaultParams is 64 MiB, 3 passes, 1 lane: the memory hardness of
	// RFC 9106's second recommended option, on one core per login.
	DefaultParams = Params{MemoryKiB: 64 * 1024, Passes: 3, Lanes: 1}
	// MinParams is the OWASP minimum. Configuration may raise the
	// parameters, never set them below this.
	MinParams = Params{MemoryKiB: 19 * 1024, Passes: 2, Lanes: 1}
	// maxParams bounds what is accepted from configuration and from a
	// stored hash, so neither can make one login use unbounded memory
	// or time.
	maxParams = Params{MemoryKiB: 1024 * 1024, Passes: 16, Lanes: 4}
)

const (
	saltLength = 16
	hashLength = 32
	// argonVersion is the only Argon2 version there is in practice (1.3).
	argonVersion = argon2.Version
	phcPrefix    = "$argon2id$"
)

// Validate checks that p is within the allowed range.
func (p Params) Validate() error {
	switch {
	case p.MemoryKiB < MinParams.MemoryKiB || p.Passes < MinParams.Passes || p.Lanes < MinParams.Lanes:
		return fmt.Errorf("argon2 parameters m=%d,t=%d,p=%d are below the minimum m=%d,t=%d,p=%d",
			p.MemoryKiB, p.Passes, p.Lanes, MinParams.MemoryKiB, MinParams.Passes, MinParams.Lanes)
	case p.MemoryKiB > maxParams.MemoryKiB || p.Passes > maxParams.Passes || p.Lanes > maxParams.Lanes:
		return fmt.Errorf("argon2 parameters m=%d,t=%d,p=%d are above the maximum m=%d,t=%d,p=%d",
			p.MemoryKiB, p.Passes, p.Lanes, maxParams.MemoryKiB, maxParams.Passes, maxParams.Lanes)
	}
	return nil
}

// weakerThan reports whether a hash made with p should be recomputed now
// that q is configured.
func (p Params) weakerThan(q Params) bool {
	return p.MemoryKiB < q.MemoryKiB || p.Passes < q.Passes || p.Lanes < q.Lanes
}

var b64 = base64.RawStdEncoding

// encodePHC formats a hash as a PHC string:
// $argon2id$v=19$m=65536,t=3,p=1$<salt>$<hash>, base64 without padding.
func encodePHC(p Params, salt, hash []byte) string {
	return fmt.Sprintf("%sv=%d$m=%d,t=%d,p=%d$%s$%s", phcPrefix, argonVersion,
		p.MemoryKiB, p.Passes, p.Lanes, b64.EncodeToString(salt), b64.EncodeToString(hash))
}

// errBadHash is returned for a stored hash that is not a well-formed
// Argon2id PHC string.
var errBadHash = errors.New("auth: stored password hash is not a valid argon2id PHC string")

// parsePHC is strict: it accepts exactly what encodePHC produces, with
// parameters inside the accepted range.
func parsePHC(s string) (p Params, salt, hash []byte, err error) {
	rest, ok := strings.CutPrefix(s, phcPrefix)
	if !ok {
		return Params{}, nil, nil, errBadHash
	}
	// The base64 decoder skips line breaks; a PHC string has none.
	if strings.ContainsAny(s, " \t\r\n") {
		return Params{}, nil, nil, errBadHash
	}
	parts := strings.Split(rest, "$")
	if len(parts) != 4 || parts[0] != "v="+strconv.Itoa(argonVersion) {
		return Params{}, nil, nil, errBadHash
	}
	fields := strings.Split(parts[1], ",")
	if len(fields) != 3 {
		return Params{}, nil, nil, errBadHash
	}
	num := func(field, key string, bits int) (uint64, bool) {
		v, ok := strings.CutPrefix(field, key+"=")
		if !ok || v == "" || v[0] == '+' || v[0] == '0' {
			return 0, false
		}
		n, err := strconv.ParseUint(v, 10, bits)
		return n, err == nil
	}
	m, ok1 := num(fields[0], "m", 32)
	t, ok2 := num(fields[1], "t", 32)
	l, ok3 := num(fields[2], "p", 8)
	if !ok1 || !ok2 || !ok3 {
		return Params{}, nil, nil, errBadHash
	}
	p = Params{MemoryKiB: uint32(m), Passes: uint32(t), Lanes: uint8(l)}
	// The lower bound for a stored hash is what Argon2 itself requires,
	// not MinParams: a hash is still verifiable after the minimum was
	// raised, and is then recomputed.
	if p.Passes < 1 || p.Lanes < 1 || p.MemoryKiB < 8*uint32(p.Lanes) ||
		p.MemoryKiB > maxParams.MemoryKiB || p.Passes > maxParams.Passes || p.Lanes > maxParams.Lanes {
		return Params{}, nil, nil, errBadHash
	}
	if salt, err = b64.Strict().DecodeString(parts[2]); err != nil || len(salt) < 8 || len(salt) > 64 {
		return Params{}, nil, nil, errBadHash
	}
	if hash, err = b64.Strict().DecodeString(parts[3]); err != nil || len(hash) < 16 || len(hash) > 64 {
		return Params{}, nil, nil, errBadHash
	}
	return p, salt, hash, nil
}

// derive computes the Argon2id hash of password.
func derive(password string, salt []byte, p Params, length int) []byte {
	return argon2.IDKey([]byte(password), salt, p.Passes, p.MemoryKiB, p.Lanes, uint32(length))
}

// hashPassword hashes password with a fresh random salt.
func hashPassword(password string, p Params) (string, error) {
	salt := make([]byte, saltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: generate salt: %w", err)
	}
	return encodePHC(p, salt, derive(password, salt, p, hashLength)), nil
}

// verifyPassword reports whether password matches the PHC string, and the
// parameters the hash was made with.
func verifyPassword(password, phc string) (bool, Params, error) {
	p, salt, want, err := parsePHC(phc)
	if err != nil {
		return false, Params{}, err
	}
	got := derive(password, salt, p, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1, p, nil
}
