package logging

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestNewJSON(t *testing.T) {
	var buf bytes.Buffer
	log, err := New(&buf, "info", "json")
	if err != nil {
		t.Fatal(err)
	}
	log.Debug("hidden")
	log.Info("hello", "component", "test")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("want exactly one line (debug filtered), got %d: %q", len(lines), buf.String())
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if rec["msg"] != "hello" || rec["component"] != "test" {
		t.Fatalf("unexpected record: %v", rec)
	}
}

func TestNewText(t *testing.T) {
	var buf bytes.Buffer
	log, err := New(&buf, "DEBUG", "TEXT")
	if err != nil {
		t.Fatal(err)
	}
	log.Debug("visible")
	if !strings.Contains(buf.String(), "msg=visible") {
		t.Fatalf("unexpected output: %q", buf.String())
	}
}

func TestNewRejectsUnknown(t *testing.T) {
	if _, err := New(&bytes.Buffer{}, "loud", "json"); err == nil {
		t.Error("unknown level accepted")
	}
	if _, err := New(&bytes.Buffer{}, "info", "xml"); err == nil {
		t.Error("unknown format accepted")
	}
}
