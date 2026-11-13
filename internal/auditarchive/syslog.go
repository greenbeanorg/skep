package auditarchive

import (
	"fmt"
	"log/slog"
	"log/syslog"
)

// SyslogNotifier returns an Options.Notify that writes each seal to the
// local syslog at daemon.notice under tag. On a host that forwards syslog
// that line is an off-host anchor for the hash chain (ADR 0007, decision
// 5): it names the month, the ID range, the count, and the hash.
//
// A seal is already complete when this runs. If syslog cannot be reached
// the failure is logged and nothing else happens.
func SyslogNotifier(tag, logName string, log *slog.Logger) func(Segment) {
	return func(seg Segment) {
		w, err := syslog.New(syslog.LOG_DAEMON|syslog.LOG_NOTICE, tag)
		if err != nil {
			log.Warn("could not write the audit seal to syslog", "error", err, "month", seg.Month, "sha256", seg.Hash)
			return
		}
		defer w.Close()
		if err := w.Notice(SealLine(logName, seg)); err != nil {
			log.Warn("could not write the audit seal to syslog", "error", err, "month", seg.Month, "sha256", seg.Hash)
		}
	}
}

// SealLine is the syslog message of a seal.
func SealLine(logName string, seg Segment) string {
	previous := seg.PreviousHash
	if previous == "" {
		previous = "none"
	}
	return fmt.Sprintf("audit segment sealed: log=%s node=%s month=%s first_id=%d last_id=%d entries=%d sha256=%s previous=%s file=%s",
		logName, seg.NodeID, seg.Month, seg.FirstID, seg.LastID, seg.Entries, seg.Hash, previous, seg.FileName)
}
