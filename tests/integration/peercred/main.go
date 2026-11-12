// Command peercred is a manual integration probe for Unix socket peer
// credentials. It exists to prove, on a real FreeBSD host, the mechanism
// that ADR 0004 relies on: the agent can learn the UID of whoever connects
// to its socket from the kernel, and refuse UIDs that are not allow-listed.
//
// Usage:
//
//	peercred serve -socket PATH -mode 0666 -allow 0,1001
//	peercred dial  -socket PATH
//
// The server prints one line per connection with the kernel-reported
// credentials and its decision, and answers the client "allowed" or
// "denied". It is a test tool and is not installed with Skep.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/greenbeanorg/skep/internal/peercred"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: peercred serve|dial -socket PATH [...]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "dial":
		err = dial(os.Args[2:])
	default:
		err = fmt.Errorf("unknown mode %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "peercred:", err)
		os.Exit(1)
	}
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	socket := fs.String("socket", "", "socket path")
	mode := fs.String("mode", "0660", "octal socket file mode")
	allow := fs.String("allow", "", "comma-separated UIDs allowed to connect")
	if err := fs.Parse(args); err != nil {
		return err
	}
	perm, err := strconv.ParseUint(*mode, 8, 32)
	if err != nil {
		return fmt.Errorf("invalid mode %q", *mode)
	}
	var uids []uint32
	for _, s := range strings.Split(*allow, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		n, err := strconv.ParseUint(s, 10, 32)
		if err != nil {
			return fmt.Errorf("invalid UID %q", s)
		}
		uids = append(uids, uint32(n))
	}

	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: *socket, Net: "unix"})
	if err != nil {
		return err
	}
	defer ln.Close()
	if err := os.Chmod(*socket, os.FileMode(perm)); err != nil {
		return err
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() { <-sig; ln.Close() }()

	fmt.Printf("serving uid=%d socket=%s mode=%04o allow=%v\n", os.Geteuid(), *socket, perm, uids)
	for {
		conn, err := ln.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		cred, err := peercred.FromConn(conn)
		verdict := "denied"
		switch {
		case err != nil:
			fmt.Printf("connection: credentials unavailable (%v) -> denied\n", err)
		case peercred.Allowed(cred, uids):
			verdict = "allowed"
			fallthrough
		default:
			fmt.Printf("connection: kernel reports uid=%d gid=%d -> %s\n", cred.UID, cred.GID, verdict)
		}
		fmt.Fprintln(conn, verdict)
		conn.Close()
	}
}

func dial(args []string) error {
	fs := flag.NewFlagSet("dial", flag.ContinueOnError)
	socket := fs.String("socket", "", "socket path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: *socket, Net: "unix"})
	if err != nil {
		return err
	}
	defer conn.Close()
	reply, err := io.ReadAll(conn)
	if err != nil {
		return err
	}
	fmt.Printf("client uid=%d: server replied %s", os.Geteuid(), reply)
	return nil
}
