package peercred

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// TestFromConnReportsOwnCredentials connects to a socket from the test
// process itself: the kernel must report this process's own UID and GID on
// both ends. This runs natively on Linux and on FreeBSD.
func TestFromConnReportsOwnCredentials(t *testing.T) {
	// A short path: sun_path is limited to about 104 bytes.
	dir, err := os.MkdirTemp("", "skep-pc")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "s"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	accepted := make(chan *net.UnixConn, 1)
	go func() {
		c, err := ln.AcceptUnix()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- c
	}()

	client, err := net.DialUnix("unix", nil, ln.Addr().(*net.UnixAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, ok := <-accepted
	if !ok {
		t.Fatal("accept failed")
	}
	defer server.Close()

	want := Cred{UID: uint32(os.Geteuid()), GID: uint32(os.Getegid())}
	for name, conn := range map[string]*net.UnixConn{"server side": server, "client side": client} {
		got, err := FromConn(conn)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != want {
			t.Errorf("%s: got %+v, want %+v", name, got, want)
		}
	}
}

func TestFromConnNil(t *testing.T) {
	if _, err := FromConn(nil); err == nil {
		t.Error("a nil connection must be an error")
	}
}

func TestAllowed(t *testing.T) {
	c := Cred{UID: 1001}
	if !Allowed(c, []uint32{0, 1001}) {
		t.Error("listed UID must be allowed")
	}
	if Allowed(c, []uint32{0}) || Allowed(c, nil) {
		t.Error("an unlisted UID, or an empty list, must be denied")
	}
	if Allowed(Cred{UID: 0}, []uint32{1001}) {
		t.Error("root is not implicitly allowed")
	}
}
