// Command consolectl attaches a terminal to a VM's serial console through
// the public API (GET /api/v1/vms/{id}/console/serial), the endpoint the
// web interface uses. It is a test harness for tests/integration/vm: run it
// in a terminal emulator (for example a tmux pane) whose line discipline is
// raw, and the guest's full-screen programs work as in a browser.
//
//	stty raw -echo; consolectl -server https://HOST:8443 -pin sha256:HEX \
//	    -token-file FILE -vm VM-ID; stty sane
//
// The server's public key is pinned (the same pin skepctl uses); there is
// no option to skip verification. The bearer token is read from a file.
// Ctrl-] ends the session.
//
// With -wait-for PATTERN -timeout D it is not interactive: it reads
// nothing from stdin, sends nothing to the guest, and waits for the
// literal PATTERN in the console text. It exits 0 when the pattern
// appears, 2 on the timeout and 1 on any other failure (the console
// closed, for example); on 2 and 1 it prints the end of the console text,
// escaped. The text is untrusted guest output: it is only matched.
package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
)

func main() {
	err := run()
	var t timeoutError
	switch {
	case errors.As(err, &t):
		fmt.Fprintln(os.Stderr, "consolectl:", err)
		os.Exit(2)
	case err != nil:
		fmt.Fprintln(os.Stderr, "\r\nconsolectl:", err)
		os.Exit(1)
	}
}

// timeoutError is a -wait-for timeout: exit code 2.
type timeoutError struct{ msg string }

func (e timeoutError) Error() string { return e.msg }

// waitMode is -wait-for: it reads the console until the pattern appears
// or the timeout passes. The console text is never written out except
// escaped, and nothing is sent to the guest.
func waitMode(ctx context.Context, conn *websocket.Conn, pattern []byte, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	tail, err := waitFor(ctx, func(ctx context.Context) ([]byte, error) {
		_, data, err := conn.Read(ctx)
		return data, err
	}, pattern)
	if err == nil {
		conn.Close(websocket.StatusNormalClosure, "")
		fmt.Println("consolectl: the pattern appeared")
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return timeoutError{fmt.Sprintf("the pattern did not appear within %s; last console text: %s", timeout, escaped(tail))}
	}
	return fmt.Errorf("%w; last console text: %s", err, escaped(tail))
}

func run() error {
	server := flag.String("server", "", "skep-api, https://HOST:PORT")
	pin := flag.String("pin", "", "the server's public key pin, sha256:HEX (skep-api tls fingerprint)")
	tokenFile := flag.String("token-file", "", "file holding a bearer session token")
	vm := flag.String("vm", "", "VM ID")
	requestID := flag.String("request-id", "", "X-Request-ID of the attachment, to find it in both audit logs")
	pattern := flag.String("wait-for", "", "not interactive: wait for this literal text in the console, then exit 0")
	timeout := flag.Duration("timeout", 0, "with -wait-for: give up after this long and exit 2")
	flag.Parse()
	if *server == "" || *pin == "" || *tokenFile == "" || *vm == "" {
		return errors.New("-server, -pin, -token-file, and -vm are required")
	}
	if (*pattern == "") != (*timeout == 0) || *timeout < 0 {
		return errors.New("-wait-for and a positive -timeout go together")
	}
	want, ok := strings.CutPrefix(*pin, "sha256:")
	if !ok || len(want) != 64 {
		return errors.New("-pin must be sha256: followed by 64 hex digits")
	}
	raw, err := os.ReadFile(*tokenFile)
	if err != nil {
		return err
	}
	token := strings.TrimSpace(string(raw))

	tlsConf := &tls.Config{
		MinVersion: tls.VersionTLS13,
		// The chain is not used: the key is pinned instead, and checked
		// below before anything is sent.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("no certificate")
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].RawSubjectPublicKeyInfo)
			if hex.EncodeToString(sum[:]) != want {
				return errors.New("the server's public key does not match the pin")
			}
			return nil
		},
	}
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConf}}
	u := "wss://" + strings.TrimPrefix(strings.TrimRight(*server, "/"), "https://") + "/api/v1/vms/" + *vm + "/console/serial"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hdr := http.Header{"Authorization": {"Bearer " + token}}
	if *requestID != "" {
		hdr.Set("X-Request-ID", *requestID)
	}
	conn, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPClient: hc, HTTPHeader: hdr})
	if err != nil {
		if resp != nil {
			return fmt.Errorf("attach: HTTP %d: %w", resp.StatusCode, err)
		}
		return fmt.Errorf("attach: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)
	if *pattern != "" {
		return waitMode(ctx, conn, []byte(*pattern), *timeout)
	}

	go func() {
		defer cancel()
		buf := make([]byte, 1024)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				return
			}
			for i := 0; i < n; i++ {
				if buf[i] == 0x1d { // Ctrl-]
					conn.Close(websocket.StatusNormalClosure, "")
					return
				}
			}
			if err := conn.Write(ctx, websocket.MessageBinary, buf[:n]); err != nil {
				return
			}
		}
	}()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			if s := websocket.CloseStatus(err); s != -1 {
				return fmt.Errorf("the console closed (%d)", s)
			}
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if _, err := os.Stdout.Write(data); err != nil {
			return err
		}
	}
}
