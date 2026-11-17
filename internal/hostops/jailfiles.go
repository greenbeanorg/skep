package hostops

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"regexp"
	"strings"
)

// What Skep writes into a jail before its first start (ADR 0010 decisions
// 12 and 13). This file holds the content and its validation; the writes
// themselves are in jailwrite_freebsd.go.

// RCConfHeader starts the rc.conf Skep writes. An rc.conf without it is
// not Skep's, and Skep refuses to replace or remove it.
const RCConfHeader = "# Written by Skep before this jail's first start (ADR 0010 decision 13).\n"

// The files Skep writes, relative to the jail root.
const (
	jailRCConfDir      = "etc"
	jailRCConfName     = "rc.conf"
	jailRootHome       = "root"
	jailSSHDir         = ".ssh"
	jailAuthorizedKeys = "authorized_keys"
)

// MaxSSHKeyLength bounds an SSH public key line.
const MaxSSHKeyLength = 16 << 10

// sshKeyTypes are the OpenSSH public key types Skep accepts.
var sshKeyTypes = map[string]bool{
	"ssh-ed25519": true, "ssh-rsa": true,
	"ecdsa-sha2-nistp256": true, "ecdsa-sha2-nistp384": true, "ecdsa-sha2-nistp521": true,
	"sk-ssh-ed25519@openssh.com": true, "sk-ecdsa-sha2-nistp256@openssh.com": true,
}

var sshCommentRE = regexp.MustCompile(`^[\x21-\x7e]([\x20-\x7e]{0,254}[\x21-\x7e])?$`)

// NormalizeSSHPublicKey checks one OpenSSH public key line, "type base64
// [comment]", and returns it with single spaces. Options (from=...,
// command=...) are refused: the line has to start with a key type. The
// base64 blob must decode and name the same type.
func NormalizeSSHPublicKey(s string) (string, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return "", invalid("the SSH public key is empty")
	case len(s) > MaxSSHKeyLength:
		return "", invalid("the SSH public key is longer than %d bytes", MaxSSHKeyLength)
	case strings.ContainsAny(s, "\r\n\x00"):
		return "", invalid("the SSH public key must be one line")
	}
	f := strings.Fields(s)
	if len(f) < 2 {
		return "", invalid("the SSH public key must be \"type base64 [comment]\"")
	}
	if !sshKeyTypes[f[0]] {
		return "", invalid("the SSH public key type must be one of ssh-ed25519, ssh-rsa, ecdsa-sha2-nistp256/384/521, or a FIDO (sk-) type; options before the key are not accepted")
	}
	blob, err := base64.StdEncoding.DecodeString(f[1])
	if err != nil || len(blob) < 4 {
		return "", invalid("the SSH public key's data is not valid base64")
	}
	n := binary.BigEndian.Uint32(blob)
	if uint64(n) > uint64(len(blob)-4) || string(blob[4:4+n]) != f[0] {
		return "", invalid("the SSH public key's data does not match its type %s", f[0])
	}
	out := f[0] + " " + f[1]
	if len(f) > 2 {
		comment := strings.Join(f[2:], " ")
		if !sshCommentRE.MatchString(comment) {
			return "", invalid("the SSH public key's comment must be printable ASCII, at most 256 characters")
		}
		out += " " + comment
	}
	return out, nil
}

var rcVarRE = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,63}="[^"\\$` + "`" + `\x00-\x1f\x7f]{0,512}"$`)

// jailContent is JailFiles rendered: the bytes to write, nil to remove.
type jailContent struct {
	rcConf         []byte
	authorizedKeys []byte
}

// render checks JailFiles and renders the files. Every rc.conf line is a
// plain name="value" assignment with no quote, backslash, dollar,
// backquote, or control character in the value, so it is data to rc.subr,
// never code.
func (f JailFiles) render() (jailContent, error) {
	var c jailContent
	if len(f.RCConf) > 0 {
		var b strings.Builder
		b.WriteString(RCConfHeader)
		for _, l := range f.RCConf {
			if !rcVarRE.MatchString(l) {
				return jailContent{}, fmt.Errorf("hostops: refusing an rc.conf line that is not a plain assignment: %q", printable(l))
			}
			b.WriteString(l + "\n")
		}
		c.rcConf = []byte(b.String())
	}
	if f.AuthorizedKey != "" {
		key, err := NormalizeSSHPublicKey(f.AuthorizedKey)
		if err != nil {
			return jailContent{}, err
		}
		c.authorizedKeys = []byte(key + "\n")
	}
	return c, nil
}

// SSHRCConf is what rc.conf needs for root's SSH key to work: sshd on,
// and root allowed with a key only. FreeBSD's sshd defaults to
// PermitRootLogin no; the flag avoids editing sshd_config.
var SSHRCConf = []string{
	`sshd_enable="YES"`,
	`sshd_flags="-o PermitRootLogin=prohibit-password"`,
}
