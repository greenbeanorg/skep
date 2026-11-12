package agentclient

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/pkg/models"
)

func TestNewValidation(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Error("an empty socket path must be refused")
	}
}

func TestAgentDownIsUnavailable(t *testing.T) {
	c, err := New(Options{SocketPath: filepath.Join(t.TempDir(), "absent.sock"), ConnectTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx := agentproto.WithCaller(context.Background(), agentproto.Caller{ActorID: "u", ActorName: "n"})

	_, err = c.System(ctx)
	var me *models.Error
	if !errors.As(err, &me) || me.Code != models.CodeAgentUnavailable {
		t.Fatalf("error = %v, want agent_unavailable", err)
	}
	h := c.Health(ctx)
	if h.Status != models.HealthDegraded || h.Checks["agent"].Status != models.HealthDegraded {
		t.Errorf("health = %+v", h)
	}
}

func TestCallerIsMandatoryOnTheClientSide(t *testing.T) {
	c, err := New(Options{SocketPath: "/nonexistent.sock"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.System(context.Background()); err == nil || errors.Is(err, ErrUnavailable) {
		t.Errorf("no caller: %v, want a refusal before any connection", err)
	}
	bad := agentproto.WithCaller(context.Background(), agentproto.Caller{ActorID: "has space", ActorName: "n"})
	if _, err := c.System(bad); err == nil || errors.Is(err, ErrUnavailable) {
		t.Errorf("invalid caller: %v", err)
	}
}

func TestResponseError(t *testing.T) {
	env := func(code string) []byte {
		return []byte(`{"error":{"code":"` + code + `","message":"m","details":{"k":"v"}}}`)
	}
	if err := responseError(http.StatusNotFound, env("not_found")); !errors.Is(err, models.ErrNotFound) {
		t.Errorf("404: %v", err)
	}
	var me *models.Error
	if err := responseError(http.StatusConflict, env(models.CodeNotCancellable)); !errors.As(err, &me) ||
		me.Code != models.CodeNotCancellable || me.Details["k"] != "v" {
		t.Errorf("409: %v", err)
	}
	for name, err := range map[string]error{
		"code with the wrong status": responseError(http.StatusBadRequest, env(models.CodeNotCancellable)),
		"internal error":             responseError(http.StatusInternalServerError, env("internal_error")),
		"forbidden":                  responseError(http.StatusForbidden, env("forbidden")),
		"not json":                   responseError(http.StatusBadGateway, []byte("<html>")),
	} {
		if err == nil || errors.As(err, &me) || errors.Is(err, models.ErrNotFound) {
			t.Errorf("%s: %v, want an opaque error", name, err)
		}
	}
}
