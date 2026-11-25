package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/localapi"
	"github.com/greenbeanorg/skep/internal/cluster/raftnet"
)

// Clock-offset health (ADR 0013 B1; D3 of step 8b-2b). Every status round
// asks each member for its clock after its status. Nothing is refused
// because of a clock in this step (forwarding's node_clock_skew is step
// 8e).
const (
	// clockWarn and clockCritical are the limits on |offset| minus the
	// sample's uncertainty.
	clockWarn     = 5 * time.Second
	clockCritical = 15 * time.Second
	// clockMaxRTT drops a sample whose round trip is longer.
	clockMaxRTT = 2 * time.Second
)

// ClockAnswer is the answer of the clock operation: the answering node's
// time in Unix milliseconds.
type ClockAnswer struct {
	NodeID     string `json:"node_id"`
	UnixMillis int64  `json:"unix_ms"`
}

// clockSample is this node's last view of one member's clock.
type clockSample struct {
	// ok is set once a sample was taken; offset and uncertainty are
	// those of the last sample kept.
	ok          bool
	offset      time.Duration
	uncertainty time.Duration
	rtt         time.Duration
	at          time.Time
	// unknown says why the last attempt gave no sample: "older build or
	// refused", "no answer", "round trip over 2 s".
	unknown string
}

// sampleClock asks one member for its clock and computes the offset: the
// remote time minus the midpoint of the local send and receive; the
// uncertainty is half the round trip. A round trip over clockMaxRTT gives
// no sample.
func (s *Service) sampleClock(ctx context.Context, m clusterstate.Member) clockSample {
	cctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	var ans ClockAnswer
	s.mu.Lock()
	op := s.clockOp
	s.mu.Unlock()
	t0 := s.now()
	err := s.caller.Call(cctx, m.Address, m.Pin, op, struct{}{}, &ans)
	t1 := s.now()
	var pe *raftnet.PeerError
	switch {
	case errors.As(err, &pe):
		// A build before 8b-2b answers "forbidden" for an operation it
		// does not know; it cannot be told from a real refusal.
		return clockSample{unknown: "older build or refused (" + pe.Code + ")"}
	case err != nil:
		return clockSample{unknown: "no answer"}
	case ans.NodeID != m.ID:
		return clockSample{unknown: "answered as another node"}
	}
	rtt := t1.Sub(t0)
	if rtt > clockMaxRTT || rtt < 0 {
		return clockSample{unknown: fmt.Sprintf("round trip %s, over %s", rtt.Round(time.Millisecond), clockMaxRTT)}
	}
	mid := t0.Add(rtt / 2)
	return clockSample{ok: true, offset: time.UnixMilli(ans.UnixMillis).Sub(mid), uncertainty: rtt / 2, rtt: rtt, at: t1}
}

// noteClocks keeps the round's samples: a new sample replaces the member's
// last one; an attempt without a sample keeps the last sample and records
// why. s.mu is held.
func (s *Service) noteClocks(samples map[string]clockSample) {
	for id, c := range samples {
		if c.ok {
			s.clocks[id] = c
			continue
		}
		old := s.clocks[id]
		old.unknown = c.unknown
		s.clocks[id] = old
	}
}

// excess is |offset| minus the uncertainty, at least 0.
func (c clockSample) excess() time.Duration {
	d := c.offset
	if d < 0 {
		d = -d
	}
	d -= c.uncertainty
	if d < 0 {
		return 0
	}
	return d
}

// clockStatus lists, per member other than this node, its clock as this
// node saw it last; a member without a sample is "not known".
func (s *Service) clockStatus(members []clusterstate.Member) []localapi.ClockStatus {
	s.mu.Lock()
	clocks := make(map[string]clockSample, len(s.clocks))
	for k, v := range s.clocks {
		clocks[k] = v
	}
	s.mu.Unlock()
	var out []localapi.ClockStatus
	for _, m := range members {
		if m.ID == s.cfg.NodeID {
			continue
		}
		c := clocks[m.ID]
		cs := localapi.ClockStatus{NodeID: m.ID, Name: m.Name, Known: c.ok, Note: c.unknown}
		if c.ok {
			cs.OffsetMillis = c.offset.Milliseconds()
			cs.UncertaintyMillis = c.uncertainty.Milliseconds()
			cs.RoundTripMillis = c.rtt.Milliseconds()
			cs.At = c.at.UTC()
		} else if cs.Note == "" {
			cs.Note = "no sample yet"
		}
		out = append(out, cs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// clockProblem is the clock health check: the members whose offset, less
// the uncertainty, is over the warning limit, and whether one is over the
// critical limit. A member that is not known is not a problem by itself.
func (s *Service) clockProblem(members []clusterstate.Member) (string, bool) {
	problem, critical := "", false
	for _, c := range s.clockStatus(members) {
		if !c.Known {
			continue
		}
		cs := clockSample{offset: time.Duration(c.OffsetMillis) * time.Millisecond, uncertainty: time.Duration(c.UncertaintyMillis) * time.Millisecond}
		ex := cs.excess()
		if ex <= clockWarn {
			continue
		}
		if ex > clockCritical {
			critical = true
		}
		problem = appendProblem(problem, fmt.Sprintf("%s's clock is %s from this node's (±%s)", c.Name, signed(cs.offset), cs.uncertainty.Round(time.Millisecond)))
	}
	if problem != "" {
		problem = "clock offsets, from this node's point of view: " + problem + fmt.Sprintf(" (warning above %s, critical above %s; check NTP on both nodes)", clockWarn, clockCritical)
	}
	return problem, critical
}

func signed(d time.Duration) string {
	sec := math.Round(d.Seconds()*1000) / 1000
	return fmt.Sprintf("%+.3f s", sec)
}
