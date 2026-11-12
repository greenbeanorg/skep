package agentproto

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestCallerRoundTrip(t *testing.T) {
	in := Caller{ActorID: "u-1", ActorName: "andy", RequestID: "req_1", SourceIP: "2001:db8::1", IdempotencyKey: "k:1"}
	h := http.Header{}
	in.SetHeaders(h)
	out, err := FromHeaders(h)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Errorf("round trip: %+v != %+v", out, in)
	}
}

func TestCallerValidation(t *testing.T) {
	ok := Caller{ActorID: "u-1", ActorName: "andy"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Caller{ActorName: "andy"}).Validate(); !errors.Is(err, ErrNoActor) {
		t.Errorf("missing actor ID: %v", err)
	}
	if err := (Caller{ActorID: "u-1"}).Validate(); !errors.Is(err, ErrNoActor) {
		t.Errorf("missing actor name: %v", err)
	}
	bad := map[string]Caller{
		"actor id":   {ActorID: "u 1", ActorName: "andy"},
		"actor name": {ActorID: "u-1", ActorName: "an\ndy"},
		"long actor": {ActorID: strings.Repeat("x", 129), ActorName: "andy"},
		"request id": {ActorID: "u-1", ActorName: "andy", RequestID: "a b"},
		"source ip":  {ActorID: "u-1", ActorName: "andy", SourceIP: "example.org"},
		"idem key":   {ActorID: "u-1", ActorName: "andy", IdempotencyKey: "a/b"},
	}
	for name, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted %+v", name, c)
		}
	}
}

func TestCallerContext(t *testing.T) {
	if _, ok := CallerFrom(context.Background()); ok {
		t.Error("an empty context has no caller")
	}
	c := Caller{ActorID: "u-1", ActorName: "andy"}
	got, ok := CallerFrom(WithCaller(context.Background(), c))
	if !ok || got != c {
		t.Errorf("got %+v, %v", got, ok)
	}
}
