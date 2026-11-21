package hostops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// Amendment r: the console log as served, and the first stderr line of
// jail -c and of a command in a build jail, are cleaned.

// dirtyStderr is a stderr line a jail's side could write: an ANSI colour
// sequence, a bidi override and pop, a C1 CSI, a zero-width space, and a
// carriage return.
const dirtyStderr = "pkg: \x1b[31mfake \u202eok\u202c\u009b2J\u200b done\r"

// assertNoUnsafe fails when s holds a control (other than those allowed)
// or format character.
func assertNoUnsafe(t *testing.T, what, s string, allowed ...rune) {
	t.Helper()
	for _, r := range s {
		if unsafeRune(r) && !strings.ContainsRune(string(allowed), r) {
			t.Fatalf("%s kept %U: %q", what, r, s)
		}
	}
}

func TestCleanText(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"plain", "Starting syslogd.\nStarting cron.\n", "Starting syslogd.\nStarting cron.\n"},
		{"newline and tab kept", "a\tb\n\n  c  \n", "a\tb\n\n  c  \n"},
		{"carriage return removed", "line one\r\nline two\r\n", "line one\nline two\n"},
		{"ANSI sequence", "\x1b[31mred\x1b[0m\n", "[31mred[0m\n"},
		{"other C0 and DEL", "a\x00b\x07c\x08d\x0be\x0cf\x7fg", "abcdefg"},
		{"C1", "a\u0080b\u0085c\u009bd\u009fe", "abcde"},
		{"bidi overrides", "x\u202a\u202b\u202c\u202d\u202ey", "xy"},
		{"bidi isolates", "x\u2066\u2067\u2068\u2069y", "xy"},
		{"other Cf", "x\u200b\u200c\u200d\u200e\u200f\u2060\ufeff\u00ady", "xy"},
		{"RLO filename", "saved invoice\u202egpj.exe\n", "saved invoicegpj.exe\n"},
		{"not trimmed or cut", "  " + strings.Repeat("x", 1000) + "  ", "  " + strings.Repeat("x", 1000) + "  "},
		{"other scripts kept", "café 日本 עברית\n", "café 日本 עברית\n"},
		{"invalid UTF-8 becomes U+FFFD", "a\x9bb\xc2", "a\ufffdb\ufffd"},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CleanText(tc.in)
			if got != tc.want {
				t.Fatalf("CleanText(%q) = %q, want %q", tc.in, got, tc.want)
			}
			assertNoUnsafe(t, "CleanText", got, '\n', '\t')
			if !utf8.ValidString(got) {
				t.Fatalf("CleanText(%q) is not valid UTF-8", tc.in)
			}
		})
	}
}

// TestCleanTextEveryRune: every control and format character goes, except
// newline and tab; every other rune is kept.
func TestCleanTextEveryRune(t *testing.T) {
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !utf8.ValidRune(r) {
			continue
		}
		in := "a" + string(r) + "b"
		got := CleanText(in)
		remove := r != '\n' && r != '\t' && (unicode.IsControl(r) || unicode.Is(unicode.Cf, r))
		if remove && got != "ab" {
			t.Fatalf("CleanText kept %U: %q", r, got)
		}
		if !remove && got != in {
			t.Fatalf("CleanText changed %U: %q", r, got)
		}
	}
}

func TestCleanStderr(t *testing.T) {
	if cleanStderr(nil) != nil {
		t.Fatal("nil became an error")
	}
	plain := errors.New("hostops: jail timed out")
	if got := cleanStderr(plain); got != plain || got.Error() != "hostops: jail timed out" {
		t.Fatalf("a non-command error changed: %v", got)
	}
	ce := &CommandError{Name: "jail", ExitCode: 1, Stderr: dirtyStderr, Err: errors.New("exit status 1")}
	got := cleanStderr(ce)
	if got != ce || ce.Stderr != "pkg: [31mfake ok2J done" {
		t.Fatalf("cleanStderr = %v, stderr %q", got, ce.Stderr)
	}
	// A wrapped command error is cleaned too, and the cut is cleanLine's.
	long := &CommandError{Name: "jexec", ExitCode: 1, Stderr: "\u202e" + strings.Repeat("é", 300)}
	wrapped := fmt.Errorf("context: %w", long)
	if got := cleanStderr(wrapped); got != wrapped || len(long.Stderr) > maxStderrLine || !utf8.ValidString(long.Stderr) ||
		strings.ContainsRune(long.Stderr, '\u202e') {
		t.Fatalf("wrapped: %q (%d bytes)", long.Stderr, len(long.Stderr))
	}
}

// TestCreateJailCleansStderr: a failed jail -c of a container start
// reports its stderr line cleaned.
func TestCreateJailCleansStderr(t *testing.T) {
	run := &script{answers: map[string]any{
		"jail -f /run/jail.conf -c skep-" + ctrA: &CommandError{Name: "jail", ExitCode: 1, Stderr: dirtyStderr, Err: errors.New("exit status 1")},
	}}
	err := createJail(context.Background(), run, "/run/jail.conf", "skep-"+ctrA)
	var ce *CommandError
	if !errors.As(err, &ce) || ce.Stderr != "pkg: [31mfake ok2J done" {
		t.Fatalf("createJail = %v", err)
	}
	assertNoUnsafe(t, "the jail -c error", err.Error())
	if c := run.envCmds[0]; !strings.HasSuffix(c.Path, "/jail") || len(c.Env) != len(jailEnv) || c.Timeout != jailCommandSlack {
		t.Fatalf("jail -c ran as %+v", c)
	}
	// The start's error, as startFailed builds it, carries the cleaned
	// line.
	serr := startFailed(jailSpec(), "jail(8) could not create the jail: ", err)
	assertNoUnsafe(t, "the start error", serr.Error())
	if !strings.Contains(serr.Error(), "fake ok2J done") {
		t.Fatalf("start error = %v", serr)
	}
	run.answers["jail -f /run/jail.conf -c skep-"+ctrA] = ""
	if err := createJail(context.Background(), run, "/run/jail.conf", "skep-"+ctrA); err != nil {
		t.Fatalf("a successful jail -c: %v", err)
	}
}

// TestCreateBuildJailCleansStderr: the same for the build jail's jail -c.
func TestCreateBuildJailCleansStderr(t *testing.T) {
	h, run, _ := templateHost(t)
	name := "skep-build-" + tplA
	root := "/skeptest/skep/templates/" + tplA
	call := "jail " + strings.Join(buildJailArgs(name, root), " ")
	run.answers[call] = &CommandError{Name: "jail", ExitCode: 1, Stderr: dirtyStderr, Err: errors.New("exit status 1")}
	err := h.createBuildJail(context.Background(), name, root)
	var ce *CommandError
	if !errors.As(err, &ce) || ce.Stderr != "pkg: [31mfake ok2J done" {
		t.Fatalf("createBuildJail = %v", err)
	}
	assertNoUnsafe(t, "the build jail -c error", err.Error())
	if got := run.calls[len(run.calls)-1]; got != call {
		t.Fatalf("ran %q, want %q", got, call)
	}
}

// TestBuildJailExecCleansStderr: a command that fails in the build jail
// reports its stderr line cleaned.
func TestBuildJailExecCleansStderr(t *testing.T) {
	h, run, _ := templateHost(t)
	run.answers["jexec -l skep-build-"+tplA+" /usr/sbin/certctl rehash"] = &CommandError{Name: "jexec", ExitCode: 1,
		Stderr: dirtyStderr, Err: errors.New("exit status 1")}
	ran, err := h.BuildJailExec(context.Background(), "skeptest", tplA, BuildCertctl)
	var ce *CommandError
	if ran || !errors.As(err, &ce) || ce.Stderr != "pkg: [31mfake ok2J done" || ce.ExitCode != 1 {
		t.Fatalf("BuildJailExec = %v, %v", ran, err)
	}
	assertNoUnsafe(t, "the build command error", err.Error())
}
