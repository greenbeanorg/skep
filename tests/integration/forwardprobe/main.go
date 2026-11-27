// Command forwardprobe is the step 8e-1 lab's case D (ADR 0013 amendment
// 8): it builds forwarding envelopes with internal/forward, signs them
// with the API node key of the host it runs on (read in place, as the
// skep account; the key is never copied or printed), reads the test
// user's ID and credential epoch from that host's api.db opened read-only,
// and sends one request per case to a target's node listener (port 8445),
// printing the status, the refusal header and one line. It is not shipped.
//
//	sudo -u skep ./forwardprobe -target 10.79.20.83:8445 -target-pin sha256:... \
//	    -target-id ID -origin-id ID -other-id ID -cluster-id ID -user admin \
//	    -guest /api/v1/containers/ID [-only valid]
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/greenbeanorg/skep/internal/cluster/nodekey"
	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/internal/forward"
)

func main() {
	var (
		target    = flag.String("target", "", "the target's node listener, ip:8445")
		targetPin = flag.String("target-pin", "", "the target's registered API pin")
		targetID  = flag.String("target-id", "", "the target's node ID")
		originID  = flag.String("origin-id", "", "this host's node ID")
		otherID   = flag.String("other-id", "", "a third member's node ID (wrong target, wrong origin)")
		clusterID = flag.String("cluster-id", "", "the cluster ID")
		user      = flag.String("user", "admin", "the user whose ID and epoch the envelope carries")
		guest     = flag.String("guest", "", "the path of one guest, e.g. /api/v1/containers/ID")
		keyDir    = flag.String("key-dir", "/var/db/skep/api/nodekey", "this host's API node key directory")
		dbPath    = flag.String("db", "/var/db/skep/api/api.db", "this host's api.db, opened read-only")
		only      = flag.String("only", "", "run one case")
	)
	flag.Parse()
	if err := run(*target, *targetPin, *targetID, *originID, *otherID, *clusterID, *user, *guest, *keyDir, *dbPath, *only); err != nil {
		fmt.Fprintln(os.Stderr, "forwardprobe:", err)
		os.Exit(1)
	}
}

type probe struct {
	key                         *nodekey.Key
	spki                        string
	target, pin                 string
	targetID, originID, cluster string
	userID                      string
	epoch                       uint64
}

func run(target, pin, targetID, originID, otherID, clusterID, user, guest, keyDir, dbPath, only string) error {
	key, err := nodekey.Load(keyDir)
	if err != nil {
		return err
	}
	der, err := key.PublicKeyDER()
	if err != nil {
		return err
	}
	ctx := context.Background()
	db, err := database.OpenReadOnly(ctx, dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	p := &probe{key: key, spki: base64.StdEncoding.EncodeToString(der), target: target, pin: pin, targetID: targetID, originID: originID, cluster: clusterID}
	if err := db.QueryRowContext(ctx, `SELECT id, credential_epoch FROM users WHERE username = ?`, user).Scan(&p.userID, &p.epoch); err != nil {
		return fmt.Errorf("user %s: %w", user, err)
	}
	fmt.Printf("probe: this host's API pin %s, user %s epoch %d, target %s\n", key.Pin(), user, p.epoch, target)
	valid := p.payload("GET", guest, nil, 0)
	validEnv, err := forward.Seal(valid, key)
	if err != nil {
		return err
	}
	type c struct {
		name, want string
		send       func() (int, string, string, error)
	}
	cases := []c{
		{"valid", "200", func() (int, string, string, error) { return p.send("GET", guest, nil, validEnv) }},
		{"the same envelope again", "replay", func() (int, string, string, error) { return p.send("GET", guest, nil, validEnv) }},
		{"issued 60 s ago", "clock", func() (int, string, string, error) {
			return p.signed("GET", guest, nil, p.payload("GET", guest, nil, -60000))
		}},
		{"issued 60 s ahead", "clock", func() (int, string, string, error) {
			return p.signed("GET", guest, nil, p.payload("GET", guest, nil, 60000))
		}},
		{"the query changed after signing", "bad_envelope", func() (int, string, string, error) {
			e, _ := forward.Seal(p.payload("GET", guest+"?a=1", nil, 0), key)
			return p.send("GET", guest+"?a=2", nil, e)
		}},
		{"the body changed after signing", "bad_envelope", func() (int, string, string, error) {
			e, _ := forward.Seal(p.payload("GET", guest, nil, 0), key)
			return p.send("GET", guest, []byte("x"), e)
		}},
		{"the method changed after signing (HEAD for GET)", "bad_envelope", func() (int, string, string, error) {
			e, _ := forward.Seal(p.payload("GET", guest, nil, 0), key)
			return p.send("HEAD", guest, nil, e)
		}},
		{"target " + otherID, "wrong_target", func() (int, string, string, error) {
			x := p.payload("GET", guest, nil, 0)
			x.Target = otherID
			return p.signed("GET", guest, nil, x)
		}},
		{"origin " + otherID, "bad_envelope", func() (int, string, string, error) {
			x := p.payload("GET", guest, nil, 0)
			x.Origin = otherID
			return p.signed("GET", guest, nil, x)
		}},
		{"version v2", "version", func() (int, string, string, error) {
			e, _ := forward.Seal(p.payload("GET", guest, nil, 0), key)
			return p.send("GET", guest, nil, "v2"+e[2:])
		}},
		{"an envelope of 5000 bytes", "version or bad_envelope", func() (int, string, string, error) {
			e, _ := forward.Seal(p.payload("GET", guest, nil, 0), key)
			return p.send("GET", guest, nil, e+strings.Repeat("A", 5000-len(e)))
		}},
		{"a path outside the list (GET /api/v1/users)", "no_route", func() (int, string, string, error) {
			return p.signed("GET", "/api/v1/users", nil, p.payload("GET", "/api/v1/users", nil, 0))
		}},
	}
	fmt.Println("| Case | Expected | Status | Skep-Forward-Refusal | Line |")
	fmt.Println("|---|---|---|---|---|")
	for _, x := range cases {
		if only != "" && x.name != only {
			continue
		}
		status, refusal, line, err := x.send()
		if err != nil {
			fmt.Printf("| %s | %s | - | - | %s |\n", x.name, x.want, oneLine(err.Error()))
			continue
		}
		fmt.Printf("| %s | %s | %d | %s | %s |\n", x.name, x.want, status, refusal, oneLine(line))
	}
	return nil
}

// payload is a valid payload for method and uri, issued shift ms from now.
func (p *probe) payload(method, uri string, body []byte, shift int64) forward.Payload {
	n := make([]byte, 16)
	_, _ = rand.Read(n)
	now := time.Now().UnixMilli() + shift
	return forward.Payload{V: 1, ClusterID: p.cluster, Origin: p.originID, OriginKey: p.spki, Target: p.targetID, UserID: p.userID,
		Epoch: p.epoch, RequestID: "probe-" + hex.EncodeToString(n[:4]), Method: method, Path: uri, BodySHA256: forward.BodyHash(body),
		IssuedAt: now, ExpiresAt: now + forward.Lifetime.Milliseconds(), Nonce: hex.EncodeToString(n)}
}

func (p *probe) signed(method, uri string, body []byte, x forward.Payload) (int, string, string, error) {
	e, err := forward.Seal(x, p.key)
	if err != nil {
		return 0, "", "", err
	}
	return p.send(method, uri, body, e)
}

// send makes one request with a new connection, presenting this host's
// API node key and accepting the target only by its API pin.
func (p *probe) send(method, uri string, body []byte, envelope string) (int, string, string, error) {
	conf := &tls.Config{MinVersion: tls.VersionTLS13, NextProtos: []string{"http/1.1"}, InsecureSkipVerify: true,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return p.key.Certificate(), nil },
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || nodekey.PinOfCertificate(cs.PeerCertificates[0]) != p.pin {
				return errors.New("the target's key is not its API pin")
			}
			return nil
		}}
	tr := &http.Transport{TLSClientConfig: conf, DisableKeepAlives: true, TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{}}
	defer tr.CloseIdleConnections()
	req, err := http.NewRequest(method, "https://"+p.target+uri, bytes.NewReader(body))
	if err != nil {
		return 0, "", "", err
	}
	req.Header.Set(forward.Header, envelope)
	resp, err := (&http.Client{Transport: tr, Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return 0, "", "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, resp.Header.Get(forward.RefusalHeader), string(b), nil
}

func oneLine(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "|", "/")
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return s
}
