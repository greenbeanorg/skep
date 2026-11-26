// Command agentsock sends one request to the agent's internal API on its
// Unix socket and prints the status and the body: a lab helper for step
// 8d-1 (the drain as root and as the skep account, a jail created
// straight on the agent). It is not shipped. Run it as the account whose
// peer UID is to be tested (sudo, or sudo -u skep).
//
//	agentsock [-socket PATH] [-actor NAME] METHOD PATH [JSON]
//
// PATH is below /internal/v1, for example /drain.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/greenbeanorg/skep/internal/agentproto"
)

func main() {
	socket := flag.String("socket", "/var/run/skep/agent.sock", "the agent's socket")
	actor := flag.String("actor", "lab-helper", "the actor named in the headers (recorded, never trusted)")
	flag.Parse()
	args := flag.Args()
	if len(args) < 2 || len(args) > 3 {
		fmt.Fprintln(os.Stderr, "usage: agentsock [-socket PATH] [-actor NAME] METHOD PATH [JSON]")
		os.Exit(2)
	}
	var body io.Reader
	if len(args) == 3 {
		body = strings.NewReader(args[2])
	}
	client := &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", *socket)
		},
	}}
	req, err := http.NewRequest(args[0], "http://agent"+agentproto.Prefix+args[1], body)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	agentproto.Caller{ActorID: *actor, ActorName: *actor}.SetHeaders(req.Header)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "request failed:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	fmt.Printf("%d %s\n", resp.StatusCode, strings.TrimSpace(string(b)))
}
