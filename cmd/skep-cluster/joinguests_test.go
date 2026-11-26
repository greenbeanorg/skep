package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/greenbeanorg/skep/internal/cluster/clusterconf"
	"github.com/greenbeanorg/skep/internal/cluster/clusterstate"
	"github.com/greenbeanorg/skep/internal/cluster/guestapi"
	"github.com/greenbeanorg/skep/internal/cluster/localapi"
	"github.com/greenbeanorg/skep/pkg/models"
)

var (
	hostVM   = clusterstate.JoinGuest{ID: "0a0a0a0a-0000-4000-8000-000000000001", Kind: clusterstate.KindVM, Name: "db1"}
	hostJail = clusterstate.JoinGuest{ID: "0a0a0a0a-0000-4000-8000-000000000002", Kind: clusterstate.KindContainer, Name: "j1"}
	hostNew  = clusterstate.JoinGuest{ID: "0a0a0a0a-0000-4000-8000-000000000003", Kind: clusterstate.KindContainer, Name: "j2"}
)

// joinWorld is a host with guests, a fake skep_cluster switched by the
// service actions, and a stage that reports res and records its input.
func joinWorld(t *testing.T, res stageResult) (*world, env, []string) {
	w := newWorld(t)
	clusterconf.Write(w.config, clusterconf.Config{NodeID: agentNode, NodeName: "smith", ListenAddress: "127.0.0.1"}, -1, -1)
	w.guests = []clusterstate.JoinGuest{hostVM, hostJail}
	g := serviceWithStatus(t, w, localapi.Status{})
	e := w.env(0)
	e.runAsAccount = func(_ context.Context, _, _ uint32, args []string, stdin []byte) ([]byte, error) {
		w.mu.Lock()
		w.stages, w.stdins = append(w.stages, args), append(w.stdins, stdin)
		w.mu.Unlock()
		b, _ := json.Marshal(res)
		return b, nil
	}
	adm := testAdmission(t)
	args := []string{"join", "-config", w.config, "-data-dir", w.dataDir, "-socket", g.path, "-wait", "1s", "-cluster-id", adm.ClusterID, mustAdmissionLine(t, adm)}
	return w, e, args
}

func drainCalls(w *world) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Join(w.drainCalls, ",")
}

// D10, D11, D13, V1: the join shows the guests, sets the drain, waits for
// the agent's tasks, reads the list again, gives it to the stage on its
// standard input (never on its command line), and clears the drain once.
func TestJoinWithGuestsDrainAndStageInput(t *testing.T) {
	w, e, args := joinWorld(t, stageResult{Outcome: "unknown", Standalone: "standalone.x"})
	err := run(context.Background(), args, e)
	var ec *exitCodeError
	if !errors.As(err, &ec) || ec.code != 3 {
		t.Fatalf("join: %v\n%s", err, w.out.String())
	}
	out := w.out.String()
	for _, want := range []string{"2 guests of this host join with it; their names must be free in the cluster",
		"This host's guests join with it: 1 VMs and 1 jails.", "  VMs: db1", "  Jails: j1",
		`on a member, "skep-cluster remove --forget-guests smith"`} {
		if !strings.Contains(out, want) {
			t.Errorf("the output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "has no VMs and no jails") {
		t.Error("the admission text still says the host has no guests")
	}
	if got := drainCalls(w); !strings.HasPrefix(got, "POST,GET") || !strings.HasSuffix(got, ",DELETE") || strings.Count(got, "DELETE") != 1 {
		t.Fatalf("drain calls %s", got)
	}
	if w.drainReason != "" || w.guestReads != 2 || strings.Join(w.serviceActions, ",") != "stop,start" {
		t.Fatalf("drain %q, %d reads, service %v", w.drainReason, w.guestReads, w.serviceActions)
	}
	if len(w.stdins) != 1 {
		t.Fatalf("%d stages", len(w.stdins))
	}
	var in stageInput
	if err := json.Unmarshal(w.stdins[0], &in); err != nil || len(in.Guests) != 2 || in.Guests[0] != hostVM || in.Guests[1] != hostJail {
		t.Fatalf("the stage's input %s: %v", w.stdins[0], err)
	}
	for _, a := range w.stages[0] {
		if strings.Contains(a, "db1") || strings.Contains(a, hostVM.ID) {
			t.Fatalf("a guest is on the stage's command line: %v", w.stages[0])
		}
	}
}

// D10 step 6: a list that changed after it was shown joins as it is now,
// with the new counts printed; a host without guests sends an empty list
// and the remove hint names the plain command.
func TestJoinListChangesAndNoGuests(t *testing.T) {
	w, e, args := joinWorld(t, stageResult{Outcome: "refused", Message: "no"})
	w.onGuestRead = func(n int) {
		if n == 2 {
			w.mu.Lock()
			w.guests = append(w.guests, hostNew)
			w.mu.Unlock()
		}
	}
	_ = run(context.Background(), args, e)
	if !strings.Contains(w.out.String(), "The host's guests changed since they were shown: 1 VMs and 2 jails join with it.") {
		t.Fatalf("output:\n%s", w.out.String())
	}
	var in stageInput
	if json.Unmarshal(w.stdins[0], &in) != nil || len(in.Guests) != 3 {
		t.Fatalf("the stage's input %s", w.stdins[0])
	}
	w, e, args = joinWorld(t, stageResult{Outcome: "unknown"})
	w.guests = nil
	_ = run(context.Background(), args, e)
	if string(w.stdins[0]) != `{"guests":[]}` || !strings.Contains(w.out.String(), `"skep-cluster remove smith"`) || !strings.Contains(w.out.String(), "This host has no VMs and no jails.") {
		t.Fatalf("no guests: %s\n%s", w.stdins[0], w.out.String())
	}
}

// D10, D11, V1: each refusal after the drain is set says that the drain
// was set and cleared and nothing else changed, and clears it once; a
// drain of another reason is a refusal that changes nothing.
func TestJoinDrainRefusals(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(w *world)
		extra []string
		cause string
		calls string
	}{
		{"another drain", func(w *world) { w.drainReason = "upgrade" }, nil, "a drain with another reason active", "POST"},
		{"setting fails", func(w *world) { w.drainSetErr = errors.New("connection reset") }, nil, "the drain was cleared if it had been set", "POST,DELETE"},
		{"tasks do not finish", func(w *world) { w.drainUnfinished = 2 }, []string{"-task-wait", "1s"}, "still has 2 unfinished tasks after 1s (-task-wait)", ""},
		{"the drain cannot be read", func(w *world) { w.drainGetErr = errors.New("agent gone") }, nil, "reading skep-agent's drain failed", "POST,GET,DELETE"},
		{"the second read fails", func(w *world) {
			w.onGuestRead = func(n int) {
				if n == 2 {
					w.guestErr = errors.New("agent gone")
				}
			}
		}, nil, "reading the guest list from skep-agent again failed", "POST,GET,DELETE"},
		{"the second read breaks a rule", func(w *world) {
			w.onGuestRead = func(n int) {
				if n == 2 {
					w.guests = append(w.guests, clusterstate.JoinGuest{ID: hostNew.ID, Kind: "container", Name: "j1"})
				}
			}
		}, nil, `the container name "j1" is in the list twice`, "POST,GET,DELETE"},
	} {
		w, e, args := joinWorld(t, stageResult{Outcome: "accepted"})
		c.setup(w)
		if c.extra != nil {
			args = append(append([]string{args[0]}, c.extra...), args[1:]...)
		}
		err := run(context.Background(), args, e)
		if err == nil || !strings.Contains(err.Error(), "refused") || !strings.Contains(err.Error(), c.cause) {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		after := c.calls != "POST" && c.calls != "POST,DELETE"
		if after && !strings.Contains(err.Error(), "the drain was set and cleared; nothing else was changed") {
			t.Errorf("%s: %v", c.name, err)
		}
		if c.name == "another drain" && (!strings.Contains(err.Error(), "nothing was changed") || w.drainReason != "upgrade") {
			t.Errorf("%s: %v, drain %q", c.name, err, w.drainReason)
		}
		got := drainCalls(w)
		if c.calls != "" && got != c.calls {
			t.Errorf("%s: drain calls %s, want %s", c.name, got, c.calls)
		}
		if after && (strings.Count(got, "DELETE") != 1 || !strings.HasSuffix(got, "DELETE")) {
			t.Errorf("%s: drain calls %s", c.name, got)
		}
		if len(w.serviceActions) != 0 || len(w.stages) != 0 {
			t.Errorf("%s: service %v, stages %v", c.name, w.serviceActions, w.stages)
		}
	}
}

// D13, V2: the stage reads its standard input bounded and strictly.
func TestStageInput(t *testing.T) {
	many := make([]clusterstate.JoinGuest, clusterstate.MaxJoinGuests+1)
	for i := range many {
		many[i] = clusterstate.JoinGuest{ID: fmt.Sprintf("0a0a0a0a-0000-4000-8000-%012d", i), Kind: "vm", Name: fmt.Sprintf("v%d", i)}
	}
	manyJSON, _ := json.Marshal(stageInput{Guests: many})
	for _, c := range []struct {
		name  string
		in    []byte
		cause string
	}{
		{"too large", append([]byte(`{"guests":[],"x":"`), bytes.Repeat([]byte("a"), maxStageInput)...), "larger than 262144 bytes"},
		{"unknown field", []byte(`{"guests":[],"owner":"x"}`), "unknown field"},
		{"two values", []byte(`{"guests":[]} {}`), "more than one JSON value"},
		{"no guests field", []byte(`{}`), `no "guests" field`},
		{"over 1000", manyJSON, "at most 1000"},
		{"a bad guest", []byte(`{"guests":[{"id":"x","kind":"vm","name":"a"}]}`), "not a lower-case UUID"},
	} {
		if _, err := readStageInput(bytes.NewReader(c.in)); err == nil || !strings.Contains(err.Error(), c.cause) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	if _, err := readStageInput(nil); err == nil {
		t.Error("no standard input was accepted")
	}
	got, err := readStageInput(strings.NewReader(`{"guests":[{"id":"` + hostVM.ID + `","kind":"vm","name":"db1"}]}`))
	if err != nil || len(got) != 1 || got[0] != hostVM {
		t.Fatalf("a valid list: %+v %v", got, err)
	}
	// Through the stage command: a refused input is the stage's error
	// outcome, and nothing is moved aside.
	w := newWorld(t)
	e := w.env(1000)
	e.stdin = strings.NewReader(`{"guests":[],"extra":1}`)
	if err := run(context.Background(), []string{stageCommand, "join", "-config", w.config, "-data-dir", w.dataDir, "-admission", "x"}, e); err != nil {
		t.Fatal(err)
	}
	var res stageResult
	if json.Unmarshal(w.out.Bytes(), &res) != nil || res.Outcome != "error" || !strings.Contains(res.Message, "unknown field") {
		t.Fatalf("stage: %s", w.out.String())
	}
}

// forgetService is a fake skep_cluster for "remove": its status lists the
// members, guests-read the inventory, and remove records the request and
// then drops the member from the status.
type forgetService struct {
	mu      sync.Mutex
	status  localapi.Status
	guests  guestapi.GuestsAnswer
	removes []localapi.RemoveRequest
}

func (f *forgetService) start(t *testing.T) string {
	path := filepath.Join(t.TempDir(), "r.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc(localapi.PathStatus, func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		json.NewEncoder(w).Encode(f.status)
	})
	mux.HandleFunc(localapi.PathGuestsRead, func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(f.guests)
	})
	mux.HandleFunc(localapi.PathRemove, func(w http.ResponseWriter, r *http.Request) {
		var req localapi.RemoveRequest
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.removes = append(f.removes, req)
		var id string
		var rest []clusterstate.Member
		for _, m := range f.status.Members {
			if m.Name == req.Name {
				id = m.ID
				continue
			}
			rest = append(rest, m)
		}
		if id == "" {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(models.ErrorResponse{Error: models.ErrorBody{Code: "not_found", Message: "there is no member named " + req.Name}})
			return
		}
		f.status.Members = rest
		json.NewEncoder(w).Encode(localapi.RemoveResult{NodeID: id, Name: req.Name, OpID: "op", ClusterID: f.status.ClusterID})
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return path
}

func newForgetService() *forgetService {
	const blaz = "0b0b0b0b-0000-4000-8000-00000000000b"
	f := &forgetService{status: localapi.Status{NodeID: agentNode, ClusterID: "7d1c2a9e-1b0f-4f3e-9a51-2c6b8e0d4f10",
		Members: []clusterstate.Member{{ID: agentNode, Name: "smith"}, {ID: blaz, Name: "blazanov"}, {ID: "0c0c0c0c-0000-4000-8000-00000000000c", Name: "star"}}}}
	f.guests.ClusterID = f.status.ClusterID
	for i, n := range []string{"j1", "j2" + evil} {
		f.guests.Guests = append(f.guests.Guests, guestapi.GuestInfo{Guest: clusterstate.Guest{ID: fmt.Sprintf("0d0d0d0d-0000-4000-8000-%012d", i), Kind: "container", Name: n, Owner: blaz}})
	}
	f.guests.Guests = append(f.guests.Guests, guestapi.GuestInfo{Guest: clusterstate.Guest{ID: "0d0d0d0d-0000-4000-8000-0000000000ff", Kind: "container", Name: "web1", Owner: "0c0c0c0c-0000-4000-8000-00000000000c"}})
	f.guests.Reservations = []guestapi.ReservationInfo{{Reservation: clusterstate.Reservation{Kind: "vm", Name: "r1", Owner: blaz, Key: "k"}}}
	return f
}

// D20: "remove --forget-guests" shows what the member owns, has the number
// of entries confirmed (-forget-count or the terminal), and sends the
// confirmed counts; a wrong number is refused naming it, nothing sent.
func TestRemoveForgetGuestsCommand(t *testing.T) {
	ctx := context.Background()
	f := newForgetService()
	path := f.start(t)
	w := newWorld(t)
	base := []string{"remove", "-socket", path, "-data-dir", w.dataDir, "-wait", "2s"}
	if err := run(ctx, append(base, "-forget-count", "3", "blazanov"), w.env(0)); err == nil || !strings.Contains(err.Error(), "-forget-count is only for --forget-guests") {
		t.Fatalf("-forget-count alone: %v", err)
	}
	if err := run(ctx, append(base, "--forget-guests", "-forget-count", "3", "blazanov"), w.env(1000)); err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("not root: %v", err)
	}
	for _, c := range [][]string{{"-forget-count", "2"}, {}} {
		w = newWorld(t)
		err := run(ctx, append(append(base, "--forget-guests"), append(c, "blazanov")...), w.env(0))
		if err == nil || !strings.Contains(err.Error(), "refused") || !strings.Contains(err.Error(), "3") || !strings.Contains(err.Error(), "nothing was changed") {
			t.Fatalf("%v: %v", c, err)
		}
	}
	if len(f.removes) != 0 {
		t.Fatalf("a refused confirmation sent %+v", f.removes)
	}
	out := w.out.String()
	for _, want := range []string{"blazanov owns 2 guests and 1 name reservations", "container j1  0d0d0d0d", "vm        r1  (name reservation)",
		"The guests themselves are not touched: they keep running on blazanov.", `After "skep-cluster leave --wipe" there, that host lists them in its own inventory again.`} {
		if !strings.Contains(out, want) {
			t.Errorf("the output lacks %q:\n%s", want, out)
		}
	}
	if hasControl(out) {
		t.Fatalf("control characters in the output: %q", out)
	}
	// The terminal: the number typed.
	w = newWorld(t)
	w.tty = &fakeTTY{Reader: *strings.NewReader("3\n")}
	if err := run(ctx, append(base, "--forget-guests", "blazanov"), w.env(0)); err != nil {
		t.Fatalf("confirmed on the terminal: %v\n%s", err, w.out.String())
	}
	if tty := w.tty.(*fakeTTY); !strings.Contains(tty.Buffer.String(), "Type the number of entries to forget (3) to confirm: ") {
		t.Fatalf("the prompt: %q", tty.Buffer.String())
	}
	if r := f.removes[0]; !r.ForgetGuests || r.Guests != 2 || r.Reservations != 1 || r.Name != "blazanov" {
		t.Fatalf("the request: %+v", r)
	}
	// A member that owns nothing: a plain removal, said so.
	w = newWorld(t)
	if err := run(ctx, append(base, "--forget-guests", "-forget-count", "0", "smith"), w.env(0)); err != nil {
		t.Fatalf("smith: %v", err)
	}
	if r := f.removes[1]; r.ForgetGuests || r.Guests != 0 || !strings.Contains(w.out.String(), "smith owns no guests and no name reservations") {
		t.Fatalf("plain removal: %+v\n%s", r, w.out.String())
	}
	// No such member: the service's error.
	w = newWorld(t)
	if err := run(ctx, append(base, "--forget-guests", "-forget-count", "1", "nobody"), w.env(0)); err == nil || !strings.Contains(err.Error(), "there is no member named nobody") {
		t.Fatalf("no such member: %v", err)
	}
}
