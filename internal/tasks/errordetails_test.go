package tasks

import (
	"slices"
	"testing"

	"github.com/greenbeanorg/skep/pkg/models"
)

// The keys reviewed as possibly holding a secret, a key path, a password,
// or unreviewed free text are never shown, for any code.
func TestErrorDetailKeysNeverListFlaggedKeys(t *testing.T) {
	flaggedEverywhere := []string{"error", "stderr", "cleanup_error", "build_error", "paths", "text", "password", "passphrase", "key", "keylocation", "token", "secret"}
	for code, keys := range ErrorDetailKeys {
		for _, k := range keys {
			if slices.Contains(flaggedEverywhere, k) {
				t.Errorf("%s lists the flagged key %q", code, k)
			}
		}
	}
	// "files" is SSH host key paths under template_build_failed.
	if slices.Contains(ErrorDetailKeys[models.CodeTemplateBuildFailed], "files") {
		t.Error("template_build_failed lists files (SSH host key paths)")
	}
}

func TestPublicErrorDetails(t *testing.T) {
	d := map[string]any{"step": "pkg install", "exit_code": 1, "stderr": "boom", "files": []any{"/etc/ssh/ssh_host_rsa_key"}, "cleanup_error": "x"}
	got := PublicErrorDetails(models.CodeTemplateBuildFailed, d)
	if len(got) != 2 || got["step"] != "pkg install" || got["exit_code"] != 1 {
		t.Errorf("template_build_failed: %v", got)
	}
	if got := PublicErrorDetails("not_reviewed", map[string]any{"a": 1}); got != nil {
		t.Errorf("an unreviewed code: %v", got)
	}
	if got := PublicErrorDetails(models.CodeTemplateHostChanged, map[string]any{"paths": []any{"/etc/master.passwd"}, "build_error": "x"}); got != nil {
		t.Errorf("template_host_changed: %v", got)
	}
	if got := PublicErrorDetails(models.CodeVMNotStopped, nil); got != nil {
		t.Errorf("no details: %v", got)
	}
	tk := Task{ErrorCode: models.CodeVMNotStopped, ErrorMessage: "m", ErrorDetails: map[string]any{"state": "running", "x": 1}}
	if e := tk.Public().Error; e == nil || len(e.Details) != 1 || e.Details["state"] != "running" {
		t.Errorf("Public: %+v", e)
	}
}
