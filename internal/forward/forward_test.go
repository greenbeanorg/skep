package forward

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/internal/cluster/nodekey"
	"github.com/greenbeanorg/skep/internal/cluster/peerapi"
	"github.com/greenbeanorg/skep/pkg/models"
)

const (
	clusterID = "c0c0c0c0-0000-4000-8000-000000000000"
	smithID   = "a1a1a1a1-0000-4000-8000-00000000000a" // the origin
	starID    = "b2b2b2b2-0000-4000-8000-00000000000b" // the receiver
	langID    = "c3c3c3c3-0000-4000-8000-00000000000c"
	userID    = "u1u1u1u1"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func newKey(t *testing.T, id string) *nodekey.Key {
	t.Helper()
	k, err := nodekey.CreatePrefix(t.TempDir(), nodekey.PrefixAPI, id, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// fakeSource answers peers-read with a fixed answer, or an error.
type fakeSource struct {
	mu  sync.Mutex
	ans peerapi.PeersAnswer
	err error
}

func (f *fakeSource) Peers(context.Context, uint64, time.Duration) (peerapi.PeersAnswer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ans, f.err
}

type users map[string]auth.User

func (u users) UserByID(_ context.Context, id string) (auth.User, error) {
	if x, ok := u[id]; ok {
		return x, nil
	}
	return auth.User{}, models.ErrNotFound
}

// rig is star (the receiver) with smith and langrishe as peers.
type rig struct {
	smith, lang, star, nodeKey *nodekey.Key
	peers                      *Peers
	src                        *fakeSource
	recv                       *Receiver
	now                        time.Time
	users                      users
}

func newRig(t *testing.T) *rig {
	r := &rig{smith: newKey(t, smithID), lang: newKey(t, langID), star: newKey(t, starID), nodeKey: newKey(t, smithID), now: time.Now()}
	r.src = &fakeSource{ans: peerapi.PeersAnswer{ClusterID: clusterID, NodeID: starID, NodeName: "star", ListenAddress: "127.0.0.1", Current: true,
		Peers: []peerapi.Peer{
			{NodeID: smithID, Name: "smith", Address: "127.0.0.1:8444", State: "member", APIPin: r.smith.Pin(), RaftServer: true},
			{NodeID: langID, Name: "langrishe", Address: "127.0.0.1:8444", State: "member", APIPin: r.lang.Pin(), RaftServer: true},
			{NodeID: starID, Name: "star", Address: "127.0.0.1:8444", State: "member", APIPin: r.star.Pin(), RaftServer: true},
		}}}
	r.peers = NewPeers(r.src, quiet, func() time.Time { return r.now })
	if err := r.peers.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.users = users{userID: {ID: userID, Username: "admin", CredentialEpoch: 3, Role: auth.Role{Name: "operator"}}}
	start := r.now
	r.recv = NewReceiver(r.peers, r.users, func() time.Time { return start })
	r.recv.now = func() time.Time { return r.now }
	return r
}

func spki(k *nodekey.Key) string {
	b, _ := k.PublicKeyDER()
	return base64.StdEncoding.EncodeToString(b)
}

func (r *rig) payload(method, uri string, body []byte) Payload {
	now := r.now.UnixMilli()
	return Payload{V: 1, ClusterID: clusterID, Origin: smithID, OriginKey: spki(r.smith), Target: starID, UserID: userID, Epoch: 3,
		RequestID: "req-1", IdempotencyKey: "", SourceIP: "192.0.2.7", Method: method, Path: uri, BodySHA256: BodyHash(body),
		IssuedAt: now, ExpiresAt: now + Lifetime.Milliseconds(), Nonce: strings.Repeat("ab", 16)}
}

// request builds a request as the node listener would see it, from conn's
// key, with the envelope p signed by signer.
func (r *rig) request(t *testing.T, conn *nodekey.Key, signer Signer, p Payload, method, uri string, body []byte) *http.Request {
	t.Helper()
	h, err := Seal(p, signer)
	if err != nil {
		t.Fatal(err)
	}
	return r.raw(conn, h, method, uri, body)
}

func (r *rig) raw(conn *nodekey.Key, envelope, method, uri string, body []byte) *http.Request {
	req := httptest.NewRequest(method, uri, strings.NewReader(string(body)))
	req.Header.Set(Header, envelope)
	leaf, _ := x509.ParseCertificate(conn.Certificate().Certificate[0])
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
	return req
}

func wantRefusal(t *testing.T, name string, a *Accepted, ref *Refusal, code string, verified bool) {
	t.Helper()
	if a != nil || ref == nil || ref.Code != code || ref.Verified != verified {
		t.Fatalf("%s: accepted %v, refusal %+v, want %s (verified %v)", name, a != nil, ref, code, verified)
	}
}

func TestEnvelopeSealParse(t *testing.T) {
	r := newRig(t)
	p := r.payload("GET", "/api/v1/containers/x", nil)
	h, err := Seal(p, r.smith)
	if err != nil {
		t.Fatal(err)
	}
	env, code := Parse(h)
	if code != "" || env.Payload != p {
		t.Fatalf("parse: %q %+v", code, env)
	}
	leaf, _ := x509.ParseCertificate(r.smith.Certificate().Certificate[0])
	other, _ := x509.ParseCertificate(r.lang.Certificate().Certificate[0])
	if !env.Verify(leaf.PublicKey.(*ecdsa.PublicKey)) || env.Verify(other.PublicKey.(*ecdsa.PublicKey)) {
		t.Fatal("verify")
	}
	parts := strings.Split(h, ".")
	raw, _ := b64.DecodeString(parts[1])
	for name, c := range map[string]struct {
		h, code string
	}{
		"empty":         {"", CodeBadEnvelope},
		"two parts":     {"v1." + parts[1], CodeBadEnvelope},
		"v2":            {"v2." + parts[1] + "." + parts[2], CodeVersion},
		"5000 bytes":    {h + strings.Repeat("A", 5000-len(h)), CodeBadEnvelope},
		"not base64":    {"v1.!!!." + parts[2], CodeBadEnvelope},
		"extra field":   {"v1." + b64.EncodeToString(append(raw[:len(raw)-1], []byte(`,"x":1}`)...)) + "." + parts[2], CodeBadEnvelope},
		"missing field": {"v1." + b64.EncodeToString([]byte(`{"v":1}`)) + "." + parts[2], CodeBadEnvelope},
	} {
		if _, got := Parse(c.h); got != c.code {
			t.Errorf("%s: %q, want %q", name, got, c.code)
		}
	}
	// v is 1, or the version is refused.
	p.V = 2
	b, _ := json.Marshal(p)
	if _, got := Parse("v1." + b64.EncodeToString(b) + "." + parts[2]); got != CodeVersion {
		t.Errorf("v 2: %q", got)
	}
}

func TestReceiverChecksInOrder(t *testing.T) {
	uri := "/api/v1/containers/x?a=1"
	body := []byte(`{"action":"stop"}`)
	cases := []struct {
		name     string
		build    func(r *rig) *http.Request
		code     string
		verified bool
	}{
		{"valid", func(r *rig) *http.Request {
			return r.request(t, r.smith, r.smith, r.payload("POST", uri, body), "POST", uri, body)
		}, "", false},
		{"a key nobody pinned", func(r *rig) *http.Request {
			k := newKey(t, smithID)
			return r.request(t, k, k, r.payload("POST", uri, body), "POST", uri, body)
		}, CodeNotMember, false},
		{"a member's node key (not its API key)", func(r *rig) *http.Request {
			return r.request(t, r.nodeKey, r.nodeKey, r.payload("POST", uri, body), "POST", uri, body)
		}, CodeNotMember, false},
		{"v2", func(r *rig) *http.Request {
			h, _ := Seal(r.payload("POST", uri, body), r.smith)
			return r.raw(r.smith, "v2"+h[2:], "POST", uri, body)
		}, CodeVersion, false},
		{"signed by another key", func(r *rig) *http.Request {
			return r.request(t, r.smith, r.lang, r.payload("POST", uri, body), "POST", uri, body)
		}, CodeBadEnvelope, false},
		{"origin_key not the connection's", func(r *rig) *http.Request {
			p := r.payload("POST", uri, body)
			p.OriginKey = spki(r.lang)
			return r.request(t, r.smith, r.smith, p, "POST", uri, body)
		}, CodeBadEnvelope, false},
		{"origin langrishe", func(r *rig) *http.Request {
			p := r.payload("POST", uri, body)
			p.Origin = langID
			return r.request(t, r.smith, r.smith, p, "POST", uri, body)
		}, CodeBadEnvelope, false},
		{"another cluster", func(r *rig) *http.Request {
			p := r.payload("POST", uri, body)
			p.ClusterID = langID
			return r.request(t, r.smith, r.smith, p, "POST", uri, body)
		}, CodeBadEnvelope, false},
		{"target langrishe", func(r *rig) *http.Request {
			p := r.payload("POST", uri, body)
			p.Target = langID
			return r.request(t, r.smith, r.smith, p, "POST", uri, body)
		}, CodeWrongTarget, true},
		{"query changed", func(r *rig) *http.Request {
			return r.request(t, r.smith, r.smith, r.payload("POST", uri, body), "POST", "/api/v1/containers/x?a=2", body)
		}, CodeBadEnvelope, true},
		{"body changed", func(r *rig) *http.Request {
			return r.request(t, r.smith, r.smith, r.payload("POST", uri, body), "POST", uri, []byte(`{"action":"start"}`))
		}, CodeBadEnvelope, true},
		{"method changed", func(r *rig) *http.Request {
			return r.request(t, r.smith, r.smith, r.payload("POST", uri, body), "PUT", uri, body)
		}, CodeBadEnvelope, true},
		{"idempotency key added", func(r *rig) *http.Request {
			req := r.request(t, r.smith, r.smith, r.payload("POST", uri, body), "POST", uri, body)
			req.Header.Set("Idempotency-Key", "k1")
			return req
		}, CodeBadEnvelope, true},
		{"expired", func(r *rig) *http.Request {
			p := r.payload("POST", uri, body)
			r.now = r.now.Add(31 * time.Second)
			_ = r.peers.Sync(context.Background())
			return r.request(t, r.smith, r.smith, p, "POST", uri, body)
		}, CodeClock, true},
		{"issued 60 s ahead", func(r *rig) *http.Request {
			p := r.payload("POST", uri, body)
			p.IssuedAt += 60000
			p.ExpiresAt += 60000
			return r.request(t, r.smith, r.smith, p, "POST", uri, body)
		}, CodeClock, true},
		{"a lifetime over 30 s", func(r *rig) *http.Request {
			p := r.payload("POST", uri, body)
			p.ExpiresAt += 1000
			return r.request(t, r.smith, r.smith, p, "POST", uri, body)
		}, CodeClock, true},
		{"issued before the receiver started", func(r *rig) *http.Request {
			p := r.payload("POST", uri, body)
			p.IssuedAt -= 2000
			p.ExpiresAt -= 2000
			return r.request(t, r.smith, r.smith, p, "POST", uri, body)
		}, CodeBeforeStart, true},
		{"unknown user", func(r *rig) *http.Request {
			delete(r.users, userID)
			return r.request(t, r.smith, r.smith, r.payload("POST", uri, body), "POST", uri, body)
		}, CodeUserUnknown, true},
		{"epoch higher (receiver behind)", func(r *rig) *http.Request {
			p := r.payload("POST", uri, body)
			p.Epoch = 4
			return r.request(t, r.smith, r.smith, p, "POST", uri, body)
		}, CodeBehind, true},
		{"epoch lower (origin behind)", func(r *rig) *http.Request {
			p := r.payload("POST", uri, body)
			p.Epoch = 2
			return r.request(t, r.smith, r.smith, p, "POST", uri, body)
		}, CodeStaleOrigin, true},
		{"equal and disabled", func(r *rig) *http.Request {
			u := r.users[userID]
			u.Disabled = true
			r.users[userID] = u
			return r.request(t, r.smith, r.smith, r.payload("POST", uri, body), "POST", uri, body)
		}, CodeStaleOrigin, true},
	}
	for _, c := range cases {
		r := newRig(t)
		req := c.build(r)
		_ = r.peers.Sync(context.Background())
		a, ref := r.recv.Check(req)
		if c.code == "" {
			if ref != nil || a == nil || a.User.Username != "admin" || string(a.Body) != string(body) {
				t.Fatalf("%s: %+v", c.name, ref)
			}
			got, _ := io.ReadAll(req.Body)
			if string(got) != string(body) {
				t.Fatalf("%s: the handler's body %q", c.name, got)
			}
			continue
		}
		wantRefusal(t, c.name, a, ref, c.code, c.verified)
		if c.verified && ref.Origin != smithID {
			t.Fatalf("%s: origin %q", c.name, ref.Origin)
		}
	}
}

func TestReplayAndBusy(t *testing.T) {
	r := newRig(t)
	p := r.payload("GET", "/api/v1/vms/x", nil)
	h, _ := Seal(p, r.smith)
	if _, ref := r.recv.Check(r.raw(r.smith, h, "GET", "/api/v1/vms/x", nil)); ref != nil {
		t.Fatal(ref)
	}
	a, ref := r.recv.Check(r.raw(r.smith, h, "GET", "/api/v1/vms/x", nil))
	wantRefusal(t, "replay", a, ref, CodeReplay, true)
	// The same nonce from another origin is another pair.
	c := newNonceCache(2)
	if c.add("a", "n", 100, 0) != "" || c.add("b", "n", 100, 0) != "" {
		t.Fatal("two pairs")
	}
	if got := c.add("c", "n", 100, 0); got != CodeBusy {
		t.Fatalf("full: %q", got)
	}
	// Kept until 15 s after the expiry, then the room is reused.
	if got := c.add("c", "n", 100, 100+nonceGraceMillis-1); got != CodeBusy {
		t.Fatalf("still kept: %q", got)
	}
	if got := c.add("c", "n", 100000, 100+nonceGraceMillis); got != "" {
		t.Fatalf("after the grace: %q", got)
	}
}

// D14, V6: every state in which no peer is usable.
func TestPeersFailClosed(t *testing.T) {
	now := time.Now()
	src := &fakeSource{}
	p := NewPeers(src, quiet, func() time.Time { return now })
	pin := "sha256:" + strings.Repeat("1", 64)
	member := peerapi.Peer{NodeID: smithID, Name: "smith", State: "member", APIPin: pin, RaftServer: true}
	if p.AcceptPin(pin) {
		t.Fatal("no answer yet: a peer is usable")
	}
	src.ans = peerapi.PeersAnswer{NodeID: starID, Current: true, Peers: []peerapi.Peer{member}}
	_ = p.Sync(context.Background())
	if !p.AcceptPin(pin) {
		t.Fatal("a fresh answer: not usable")
	}
	now = now.Add(16 * time.Second)
	if p.AcceptPin(pin) || len(p.Usable()) != 0 || !strings.Contains(p.Problem(), "older than 15 s") {
		t.Fatal("an answer older than 15 s: usable")
	}
	_ = p.Sync(context.Background())
	src.err = errors.New("socket gone")
	_ = p.Sync(context.Background())
	if p.AcceptPin(pin) {
		t.Fatal("after an error: usable")
	}
	src.err = nil
	for name, mut := range map[string]func(*peerapi.Peer){
		"joining":          func(x *peerapi.Peer) { x.State = "joining" },
		"removing":         func(x *peerapi.Peer) { x.State = "removing" },
		"no API pin":       func(x *peerapi.Peer) { x.APIPin = "" },
		"not a server":     func(x *peerapi.Peer) { x.RaftServer = false },
		"this node itself": func(x *peerapi.Peer) { x.NodeID = starID },
	} {
		m := member
		mut(&m)
		src.ans.Peers = []peerapi.Peer{m}
		_ = p.Sync(context.Background())
		if p.AcceptPin(pin) || peerapi.Usable(m, starID) || peerapi.WhyNotUsable(m, starID) == "" {
			t.Errorf("%s: usable", name)
		}
	}
	// A node that is joining, removed, stalled or not current answers no
	// peers (the service's side); an empty list accepts nobody.
	src.ans.Peers = nil
	_ = p.Sync(context.Background())
	if p.AcceptPin(pin) {
		t.Fatal("an empty list: usable")
	}
}

// listen opens star's node listener on loopback with handler.
func listen(t *testing.T, r *rig, handler http.Handler) *NodeListener {
	t.Helper()
	l, err := ListenNode(netip.MustParseAddrPort("127.0.0.1:0"), r.star.Certificate, r.peers.AcceptPin, handler, time.Now, quiet)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func dialWith(addr string, k *nodekey.Key) error {
	conf := &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true}
	if k != nil {
		conf.Certificates = []tls.Certificate{*k.Certificate()}
	}
	c, err := tls.Dial("tcp", addr, conf)
	if err != nil {
		return err
	}
	defer c.Close()
	_, _ = c.Write([]byte("GET /api/v1/vms/x HTTP/1.1\r\nHost: x\r\n\r\n"))
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	b := make([]byte, 64)
	_, err = c.Read(b)
	return err
}

// D17, D46: the node listener refuses, in the handshake, a connection
// without a certificate, an unpinned key, a member's node key, and the API
// key of a joining or removing member.
func TestNodeListenerHandshake(t *testing.T) {
	r := newRig(t)
	served := atomic.Int32{}
	l := listen(t, r, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { served.Add(1) }))
	addr := l.BoundAddr()
	if err := dialWith(addr, r.smith); err != nil {
		t.Fatalf("a usable peer's API key: %v", err)
	}
	joiner, remover := newKey(t, "j"), newKey(t, "r")
	r.src.ans.Peers = append(r.src.ans.Peers,
		peerapi.Peer{NodeID: "j", Name: "j", State: "joining", APIPin: joiner.Pin(), RaftServer: true},
		peerapi.Peer{NodeID: "r", Name: "r", State: "removing", APIPin: remover.Pin(), RaftServer: true})
	_ = r.peers.Sync(context.Background())
	for name, k := range map[string]*nodekey.Key{"no certificate": nil, "unpinned": newKey(t, "x"), "a member's node key": r.nodeKey,
		"a joining member's API key": joiner, "a removing member's API key": remover} {
		err := dialWith(addr, k)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if k != nil && !refusedKey(err) {
			t.Errorf("%s: %v (not the handshake's refusal)", name, err)
		}
	}
	if served.Load() != 1 {
		t.Fatalf("served %d requests", served.Load())
	}
}

// fakeIdentity is the origin's key.
type fakeIdentity struct{ k *nodekey.Key }

func (f fakeIdentity) Key() *nodekey.Key { return f.k }

// originRig is smith sending to star's real node listener.
func originRig(t *testing.T, handler http.Handler) (*Client, peerapi.Peer, *rig) {
	r := newRig(t)
	l := listen(t, r, handler)
	src := &fakeSource{ans: peerapi.PeersAnswer{ClusterID: clusterID, NodeID: smithID, NodeName: "smith", Current: true, Peers: r.src.ans.Peers}}
	op := NewPeers(src, quiet, nil)
	_ = op.Sync(context.Background())
	c := NewClient(fakeIdentity{r.smith}, op, nil)
	_, port, _ := strings.Cut(l.BoundAddr(), ":")
	c.port = atoi(port)
	return c, r.src.ans.Peers[2], r
}

func atoi(s string) int {
	n := 0
	for _, ch := range s {
		n = n*10 + int(ch-'0')
	}
	return n
}

// D35: the origin's mapping of every outcome, with the repeat counts.
func TestClientMapping(t *testing.T) {
	type want struct {
		code, outcome string
		status        int
		minA, maxA    int
	}
	refusal := func(code string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { WriteRefusal(w, r, code, "no") })
	}
	cases := map[string]struct {
		h http.Handler
		w want
	}{
		"behind":       {refusal(CodeBehind), want{models.CodeNodeBehind, "not_written", 503, 9, 11}},
		"user_unknown": {refusal(CodeUserUnknown), want{models.CodeNodeBehind, "not_written", 503, 9, 11}},
		"before_start": {refusal(CodeBeforeStart), want{models.CodeNodeUnreachable, "not_written", 503, 9, 11}},
		"stale_origin": {refusal(CodeStaleOrigin), want{auth.CodeUnauthenticated, "", 401, 1, 1}},
		"clock":        {refusal(CodeClock), want{models.CodeNodeClockSkew, "not_written", 503, 1, 1}},
		"version":      {refusal(CodeVersion), want{models.CodeNodeVersionMismatch, "not_written", 503, 1, 1}},
		"no_route":     {refusal(CodeNoRoute), want{models.CodeNodeVersionMismatch, "not_written", 503, 1, 1}},
		"replay":       {refusal(CodeReplay), want{models.CodeNodeUnreachable, "not_written", 503, 1, 1}},
		"not_member":   {refusal(CodeNotMember), want{models.CodeNodeUnreachable, "not_written", 503, 1, 1}},
		"no answer": {http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, _, _ := http.NewResponseController(w).Hijack()
			c.Close()
		}), want{models.CodeNodeUnreachable, "unknown", 503, 1, 1}},
		"handler's answer": {http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "/x")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"t"}`))
		}), want{"", "", 202, 1, 1}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cl, star, _ := originRig(t, c.h)
			res := cl.Forward(context.Background(), star, Request{Method: "POST", URI: "/api/v1/vms/x/actions", Body: []byte("{}"), UserID: userID, Epoch: 3})
			if res.HTTPStatus() != c.w.status || res.Attempts < c.w.minA || res.Attempts > c.w.maxA {
				t.Fatalf("status %d, %d attempts, err %+v", res.HTTPStatus(), res.Attempts, res.Err)
			}
			if c.w.code == "" {
				if res.Err != nil || res.Header.Get("Location") != "/x" || string(res.Body) != `{"id":"t"}` {
					t.Fatalf("passthrough %+v", res)
				}
				return
			}
			if res.Err == nil || res.Err.Code != c.w.code {
				t.Fatalf("err %+v", res.Err)
			}
			if c.w.outcome != "" && (res.Err.Details["outcome"] != c.w.outcome || res.Err.Details["node_id"] != starID || res.Err.Details["node_name"] != "star") {
				t.Fatalf("details %+v", res.Err.Details)
			}
			if name == "replay" && res.Err.Details["reason"] != CodeReplay {
				t.Fatalf("reason %+v", res.Err.Details)
			}
		})
	}
	// Nothing sent: a closed port, and a receiver that refuses this key:
	// one more attempt, then node_unreachable, not_written.
	t.Run("not sent", func(t *testing.T) {
		cl, star, r := originRig(t, http.NotFoundHandler())
		r.src.ans.Peers = nil // star accepts nobody now
		_ = r.peers.Sync(context.Background())
		for name, port := range map[string]int{"refused key": cl.port, "closed port": 1} {
			cl.port = port
			res := cl.Forward(context.Background(), star, Request{Method: "GET", URI: "/api/v1/vms/x", UserID: userID})
			if res.Attempts != 2 || res.Err == nil || res.Err.Code != models.CodeNodeUnreachable || res.Err.Details["outcome"] != "not_written" {
				t.Fatalf("%s: %d attempts, %+v", name, res.Attempts, res.Err)
			}
		}
	})
}

// D22, V5: besides the envelope the origin sends only Content-Type,
// Idempotency-Key and X-Request-ID; never Cookie, Authorization, Origin.
func TestOriginSendsNoCredentials(t *testing.T) {
	var got http.Header
	var mu sync.Mutex
	cl, star, _ := originRig(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = r.Header.Clone()
		mu.Unlock()
	}))
	res := cl.Forward(context.Background(), star, Request{Method: "POST", URI: "/api/v1/vms/x/actions", Body: []byte(`{}`),
		ContentType: "application/json", IdempotencyKey: "k1", RequestID: "r1", UserID: userID})
	if res.Err != nil || res.Status != 200 {
		t.Fatalf("%+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	for k := range got {
		switch k {
		case Header, "Content-Type", "Idempotency-Key", "X-Request-Id", "Content-Length", "Connection":
		default:
			t.Errorf("the origin sent %s", k)
		}
	}
	if got.Get("Content-Type") != "application/json" || got.Get("Idempotency-Key") != "k1" || got.Get("X-Request-Id") != "r1" || got.Get(Header) == "" {
		t.Fatalf("headers %v", got)
	}
}

func TestClockSkew(t *testing.T) {
	p := peerapi.Peer{NodeID: starID, Name: "star", Clock: peerapi.Clock{Known: true, OffsetMillis: -15500, UncertaintyMillis: 400}}
	if e := ClockSkew(p); e == nil || e.Code != models.CodeNodeClockSkew {
		t.Fatalf("15.1 s: %v", e)
	}
	p.Clock.UncertaintyMillis = 600
	if e := ClockSkew(p); e != nil {
		t.Fatalf("14.9 s: %v", e)
	}
	p.Clock = peerapi.Clock{OffsetMillis: 60000}
	if e := ClockSkew(p); e != nil {
		t.Fatalf("unknown: %v", e)
	}
}
