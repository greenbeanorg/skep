package models

import "testing"

func TestHealthRecord(t *testing.T) {
	var h Health
	h.Record("audit_archive", false, "")
	if h.Status != HealthOK || h.Checks["audit_archive"] != (HealthCheck{Status: HealthOK}) {
		t.Fatalf("after a passing check: %+v", h)
	}
	h.Record("audit_archive", false, "late")
	if h.Status != HealthDegraded || h.Checks["audit_archive"] != (HealthCheck{Status: HealthDegraded, Error: "late"}) {
		t.Fatalf("after a failing non-critical check: %+v", h)
	}
	h.Record("agent", true, "")
	if h.Status != HealthDegraded || h.Checks["agent"] != (HealthCheck{Status: HealthOK, Critical: true}) {
		t.Fatalf("a passing critical check must not hide the degradation: %+v", h)
	}
	h.Record("database", true, "down")
	if h.Status != HealthUnavailable || h.Checks["database"] != (HealthCheck{Status: HealthUnavailable, Critical: true, Error: "down"}) {
		t.Fatalf("after a failing critical check: %+v", h)
	}
	h.Record("other", false, "x")
	if h.Status != HealthUnavailable {
		t.Fatalf("a degraded check must not lift unavailable: %+v", h)
	}
}

func TestWorseHealth(t *testing.T) {
	for _, tc := range []struct{ a, b, want string }{
		{"", "", HealthOK},
		{"bogus", HealthOK, HealthOK},
		{HealthOK, HealthDegraded, HealthDegraded},
		{HealthDegraded, HealthOK, HealthDegraded},
		{HealthDegraded, HealthUnavailable, HealthUnavailable},
		{HealthUnavailable, HealthDegraded, HealthUnavailable},
	} {
		if got := WorseHealth(tc.a, tc.b); got != tc.want {
			t.Errorf("WorseHealth(%q, %q) = %q, want %q", tc.a, tc.b, got, tc.want)
		}
	}
}
