package service

import (
	"fmt"
	"os"

	"github.com/greenbeanorg/skep/internal/cluster/audit"
	"github.com/greenbeanorg/skep/internal/cluster/layout"
	"github.com/greenbeanorg/skep/internal/cluster/membership"
	"github.com/greenbeanorg/skep/internal/cluster/store"
)

// stageEvent writes a local event to the cluster audit from a stage, which
// runs as the service account while the service is stopped (the audit's
// lock makes it the only writer). The step it records has already
// happened; a failed write is printed on the stage's stderr, which reaches
// the root command's terminal, and changes nothing else.
func stageEvent(dataDir, kind string, details map[string]string) {
	err := audit.WithLog(dataDir, func(l *audit.Log) error { return l.Event(kind, details) })
	if err != nil {
		fmt.Fprintf(os.Stderr, "skep-cluster: warning: the cluster audit did not record %s: %v\n", kind, err)
	}
}

// StageMark writes a reset mark (audit.Mark) from a stage or a command
// running as the service account with the service stopped. With withKey
// the mark carries the key of the node's state now: the last index of the
// log in state/ and the epoch of state/membership.json, so that the
// service's next start does not record that log's entries again. A
// failure is printed on stderr, as for stageEvent.
func StageMark(dataDir, kind string, withKey bool, details map[string]string) {
	var epoch, index uint64
	if withKey {
		var err error
		if index, err = store.LastIndex(layout.State(dataDir)); err != nil {
			fmt.Fprintf(os.Stderr, "skep-cluster: warning: the cluster audit did not record %s: reading the log: %v\n", kind, err)
			return
		}
		if f, err := membership.Read(layout.State(dataDir)); err == nil && f != nil {
			epoch = f.Epoch
		} else if index > 0 {
			epoch = 1
		}
	}
	err := audit.WithLog(dataDir, func(l *audit.Log) error { return l.Mark(kind, epoch, index, details) })
	if err != nil {
		fmt.Fprintf(os.Stderr, "skep-cluster: warning: the cluster audit did not record %s: %v\n", kind, err)
	}
}
