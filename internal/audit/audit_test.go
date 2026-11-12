package audit

import (
	"errors"
	"testing"
)

func TestEntryValidate(t *testing.T) {
	valid := Entry{ActorID: "u1", ActorName: "andy", Action: "vm.start", Phase: Requested, Outcome: Success}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid entry rejected: %v", err)
	}

	noID, noName := valid, valid
	noID.ActorID, noName.ActorName = "", ""
	for name, e := range map[string]Entry{"no actor id": noID, "no actor name": noName} {
		if err := e.Validate(); !errors.Is(err, ErrActorRequired) {
			t.Errorf("%s: error = %v, want ErrActorRequired", name, err)
		}
	}

	noAction, badPhase, badOutcome := valid, valid, valid
	noAction.Action, badPhase.Phase, badOutcome.Outcome = "", "later", "maybe"
	for name, e := range map[string]Entry{"no action": noAction, "bad phase": badPhase, "bad outcome": badOutcome} {
		if err := e.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
