package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentRetentionDefaults(t *testing.T) {
	cfg, err := Load(LoadOptions{DefaultPath: noDefault, LookupEnv: noEnv})
	if err != nil {
		t.Fatal(err)
	}
	r := cfg.Retention
	// Tasks 90 days; the audit log sealed and never removed (ADR 0007,
	// amendment a).
	if *r.TasksDays != 90 || *r.AuditLiveDays != 0 || !*r.AuditArchive || !r.ArchiveEnabled() || r.AuditLiveDaysOrZero() != 0 {
		t.Errorf("retention = tasks %d, live %d, archive %v", *r.TasksDays, *r.AuditLiveDays, *r.AuditArchive)
	}
	if want := filepath.Join(DefaultDataDir, "agent", "audit-archive"); cfg.AuditArchiveDir() != want {
		t.Errorf("archive directory = %q, want %q", cfg.AuditArchiveDir(), want)
	}
}

func TestAgentRetentionSettings(t *testing.T) {
	load := func(retention string) (Config, error) {
		t.Helper()
		return Load(LoadOptions{Path: writeFile(t, `{"retention": `+retention+`}`), LookupEnv: noEnv})
	}

	// An explicit zero is "never", not "unset".
	cfg, err := load(`{"tasks_days": 0, "audit_live_days": 0}`)
	if err != nil || *cfg.Retention.TasksDays != 0 || *cfg.Retention.AuditLiveDays != 0 {
		t.Errorf("explicit zeros: %+v, %v", cfg.Retention, err)
	}
	// The floors are accepted, one below is not.
	if cfg, err := load(`{"tasks_days": 7, "audit_live_days": 35}`); err != nil ||
		*cfg.Retention.TasksDays != 7 || cfg.Retention.AuditLiveDaysOrZero() != 35 {
		t.Errorf("the floors: %+v, %v", cfg.Retention, err)
	}
	for retention, want := range map[string]string{
		`{"tasks_days": 6}`:           "retention.tasks_days",
		`{"tasks_days": 1}`:           "retention.tasks_days",
		`{"tasks_days": -1}`:          "retention.tasks_days",
		`{"tasks_days": 40000}`:       "retention.tasks_days",
		`{"audit_live_days": 34}`:     "retention.audit_live_days",
		`{"audit_live_days": 1}`:      "retention.audit_live_days",
		`{"audit_live_days": -5}`:     "retention.audit_live_days",
		`{"audit_live_days": "90"}`:   "parse",
		`{"audit_archive": "yes"}`:    "parse",
		`{"audit_archive_dir": "/x"}`: "unknown field",
	} {
		if _, err := load(retention); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error = %v, want one about %s", retention, err, want)
		}
	}

	// With archiving off nothing is ever removed, whatever the number says.
	cfg, err = load(`{"audit_archive": false, "audit_live_days": 400}`)
	if err != nil || cfg.Retention.ArchiveEnabled() || cfg.Retention.AuditLiveDaysOrZero() != 0 {
		t.Errorf("archive off: %+v, %v", cfg.Retention, err)
	}
	// The other settings keep their defaults.
	if *cfg.Retention.TasksDays != 90 {
		t.Errorf("tasks_days = %d, want the default", *cfg.Retention.TasksDays)
	}
}

func TestAPIRetention(t *testing.T) {
	cfg, err := loadAPI(t, APILoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The access log: sealed, and 90 days live (ADR 0007, amendment a).
	r := cfg.Retention
	if r.TasksDays != nil || *r.AuditLiveDays != 90 || !r.ArchiveEnabled() || r.AuditLiveDaysOrZero() != 90 {
		t.Errorf("retention = %+v", r)
	}
	if want := filepath.Join(DefaultDataDir, "api", "audit-archive"); cfg.AuditArchiveDir() != want {
		t.Errorf("archive directory = %q, want %q", cfg.AuditArchiveDir(), want)
	}

	load := func(retention string) (API, error) {
		t.Helper()
		return loadAPI(t, APILoadOptions{Path: writeFile(t, `{"retention": `+retention+`}`)})
	}
	if cfg, err := load(`{"audit_live_days": 0}`); err != nil || cfg.Retention.AuditLiveDaysOrZero() != 0 || !cfg.Retention.ArchiveEnabled() {
		t.Errorf("seal and never remove: %+v, %v", cfg.Retention, err)
	}
	if cfg, err := load(`{"audit_archive": false}`); err != nil || cfg.Retention.ArchiveEnabled() || cfg.Retention.AuditLiveDaysOrZero() != 0 {
		t.Errorf("archive off: %+v, %v", cfg.Retention, err)
	}
	for retention, want := range map[string]string{
		`{"audit_live_days": 34}`: "retention.audit_live_days",
		`{"audit_live_days": 7}`:  "retention.audit_live_days",
		`{"tasks_days": 90}`:      "skep-agent",
		`{"tasks_days": 0}`:       "skep-agent",
	} {
		if _, err := load(retention); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error = %v, want one about %s", retention, err, want)
		}
	}
}
