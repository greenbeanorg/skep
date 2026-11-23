package tasks

import (
	"slices"
	"testing"

	"github.com/greenbeanorg/skep/pkg/models"
)

// The keys reviewed as possibly holding a secret, a key path, a password,
// or unreviewed free text are never shown, for any code.
func TestErrorDetailKeysNeverListFlaggedKeys(t *testing.T) {
	flaggedEverywhere := []string{"error", "stderr", "cleanup_error", "build_error", "paths", "text", "password", "passphrase", "key", "keylocation", "token", "secret", "seen_on", "state_detail"}
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

// seen_on is built from ifconfig's output: it stays in the audit log.
func TestPublicErrorDetailsDropsSeenOn(t *testing.T) {
	got := PublicErrorDetails(models.CodeContainerMACInUse, map[string]any{
		"slot": 0, "mac_address": "02:00:00:00:00:01", "bridge": "bridge1",
		"seen_on": "bridge1's address table (learnt on vtnet1)",
	})
	if _, ok := got["seen_on"]; ok {
		t.Errorf("seen_on left the agent: %v", got)
	}
	if got["bridge"] != "bridge1" || got["mac_address"] != "02:00:00:00:00:01" {
		t.Errorf("the other keys must stay: %v", got)
	}
}

// state_detail can hold a failed command's text: it stays in the audit log.
func TestPublicErrorDetailsDropsStateDetail(t *testing.T) {
	got := PublicErrorDetails(models.CodeContainerQuarantined, map[string]any{
		"container_id": "c1", "state_detail": "removing it failed: jail: /secret/path",
	})
	if len(got) != 1 || got["container_id"] != "c1" {
		t.Errorf("want only container_id: %v", got)
	}
}

// volmode is shown only when it is a value ZFS defines.
func TestPublicErrorDetailsVolmodeValues(t *testing.T) {
	for _, v := range []string{"default", "geom", "dev", "none"} {
		got := PublicErrorDetails(models.CodeDiskDeviceMissing, map[string]any{"dataset": "p/d", "volmode": v})
		if got["volmode"] != v {
			t.Errorf("volmode %q must be shown: %v", v, got)
		}
	}
	for _, v := range []any{"", "DEV", "dev ", "dev\n", "full", "/etc/key", 1, true, nil, []any{"dev"}} {
		got := PublicErrorDetails(models.CodeDiskDeviceMissing, map[string]any{"dataset": "p/d", "reason": "volmode_not_dev", "volmode": v})
		if _, ok := got["volmode"]; ok {
			t.Errorf("volmode %#v must be dropped: %v", v, got)
		}
		if got["dataset"] != "p/d" || got["reason"] != "volmode_not_dev" {
			t.Errorf("the other keys must stay for %#v: %v", v, got)
		}
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
