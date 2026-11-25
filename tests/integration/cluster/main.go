// Command cluster-probe is a test tool for the native validation of
// skep-cluster on a FreeBSD lab VM (tests/integration/cluster/results). It
// is never installed. It does two things a lab host has no tool for:
//
//	cluster-probe get -socket PATH -path /cluster/v1/status
//	    one GET on a Unix socket, as whoever runs it; prints the HTTP
//	    status and body, or the connection error
//	cluster-probe tls -addr IP:8444 -version 1.2|1.3
//	    one TLS handshake with a key made for this run, which no
//	    membership pins; prints whether the listener refused it
//	cluster-probe keygen -dir DIR -name NAME -address IP:8444
//	    makes a node key in DIR (a new directory) and prints a join
//	    request for it, to be admitted and then used by "rpc"
//	cluster-probe rpc -addr IP:8444 -server-pin PIN -key-dir DIR
//	                  -claim-id ID [-claim-addr IP:PORT] -op raft|status
//	    one RPC with the key in DIR (the probe's own from keygen, or a
//	    member's data directory, read only), claiming the given node
//	    ID; prints ACCEPTED or REFUSED and why. "raft" is a heartbeat
//	    (AppendEntries), "status" Skep's status RPC
//	cluster-probe request -id ID -name NAME -address IP:8444 -pin PIN
//	    prints a join request line for these public values (for keys
//	    nobody holds, and for duplicate cases)
//	cluster-probe admission -cluster-id ID -for ID,NAME,ADDR,PIN
//	                        -member ID,NAME,ADDR,PIN [-member ...]
//	    prints an admission line for these public values (for a wrong
//	    pin, or an admission naming a member)
//
// It writes only the directory keygen is given, and changes nothing in
// any Skep file.
package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/hashicorp/go-msgpack/v2/codec"
	"github.com/hashicorp/raft"

	"github.com/greenbeanorg/skep/internal/cluster/joincode"
	"github.com/greenbeanorg/skep/internal/cluster/nodekey"
	"github.com/greenbeanorg/skep/internal/cluster/raftnet"
	"github.com/greenbeanorg/skep/internal/ids"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: cluster-probe get|tls|keygen|rpc ...")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "get":
		err = get(os.Args[2:])
	case "tls":
		err = tlsProbe(os.Args[2:])
	case "keygen":
		err = keygen(os.Args[2:])
	case "rpc":
		err = rpcProbe(os.Args[2:])
	case "request":
		err = requestLine(os.Args[2:])
	case "admission":
		err = admissionLine(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "cluster-probe:", err)
		os.Exit(2)
	}
}

// get prints "HTTP <status>" and the body, or "NO ANSWER: <error>".
func get(args []string) error {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	socket := fs.String("socket", "", "Unix socket")
	path := fs.String("path", "", "request path")
	actor := fs.String("actor", "", "value for the agent's actor headers, when the socket is the agent's")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *socket == "" || *path == "" {
		return errors.New("get needs -socket and -path")
	}
	hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", *socket)
		}}}
	req, err := http.NewRequest(http.MethodGet, "http://probe"+*path, nil)
	if err != nil {
		return err
	}
	if *actor != "" {
		req.Header.Set("Skep-Actor-Id", *actor)
		req.Header.Set("Skep-Actor-Name", *actor)
	}
	fmt.Printf("uid %d: GET %s on %s\n", os.Getuid(), *path, *socket)
	resp, err := hc.Do(req)
	if err != nil {
		fmt.Printf("NO ANSWER: %v\n", err)
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	fmt.Printf("HTTP %d\n%s\n", resp.StatusCode, body)
	return nil
}

// tlsProbe prints "REFUSED: <error>" or "ACCEPTED".
func tlsProbe(args []string) error {
	fs := flag.NewFlagSet("tls", flag.ContinueOnError)
	addr := fs.String("addr", "", "address and port")
	ver := fs.String("version", "1.3", "1.2 or 1.3")
	if err := fs.Parse(args); err != nil {
		return err
	}
	versions := map[string]uint16{"1.2": tls.VersionTLS12, "1.3": tls.VersionTLS13}
	v, ok := versions[*ver]
	if !ok || *addr == "" {
		return errors.New("tls needs -addr and -version 1.2 or 1.3")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "cluster-probe, an unknown key"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	conf := &tls.Config{
		// The probe tests what the server does with this client; it
		// does not need to trust the server.
		InsecureSkipVerify: true,
		MinVersion:         v,
		MaxVersion:         v,
		Certificates:       []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
	}
	fmt.Printf("TLS %s to %s with an unknown client key\n", *ver, *addr)
	c, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", *addr, conf)
	if err != nil {
		fmt.Printf("REFUSED: %v\n", err)
		return nil
	}
	defer c.Close()
	// A TLS 1.3 client learns of a refused certificate at its first
	// read, after its own side of the handshake is done.
	c.SetDeadline(time.Now().Add(5 * time.Second))
	_, err = c.Read(make([]byte, 1))
	switch {
	case errors.Is(err, io.EOF):
		fmt.Println("ACCEPTED: the handshake completed and the server closed the connection")
	case err != nil:
		fmt.Printf("REFUSED: %v\n", err)
	default:
		fmt.Println("ACCEPTED: the server sent data")
	}
	return nil
}

// keygen makes a key the way skep-cluster does and prints its request.
func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	dir := fs.String("dir", "", "new directory for the key")
	name := fs.String("name", "", "node name for the request")
	address := fs.String("address", "", "address for the request (ip:8444)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" || *name == "" || *address == "" {
		return errors.New("keygen needs -dir, -name and -address")
	}
	if err := os.Mkdir(*dir, 0o700); err != nil {
		return err
	}
	id, err := ids.NewUUID()
	if err != nil {
		return err
	}
	k, err := nodekey.Create(*dir, id, time.Now())
	if err != nil {
		return err
	}
	if err := os.WriteFile(*dir+"/node-id", []byte(id+"\n"), 0o600); err != nil {
		return err
	}
	line, err := joincode.EncodeRequest(joincode.Request{Node: joincode.Node{NodeID: id, Name: *name, Address: *address, Pin: k.Pin()}})
	if err != nil {
		return err
	}
	fmt.Printf("node ID %s\npin %s\n%s\n", id, k.Pin(), line)
	return nil
}

// rpcProbe sends one RPC claiming a node ID and prints what happened.
func rpcProbe(args []string) error {
	fs := flag.NewFlagSet("rpc", flag.ContinueOnError)
	addr := fs.String("addr", "", "target ip:8444")
	serverPin := fs.String("server-pin", "", "the target's pin (skep-cluster status on the target)")
	keyDir := fs.String("key-dir", "", "directory with node.key and node.crt")
	claimID := fs.String("claim-id", "", "node ID to put in the request")
	claimAddr := fs.String("claim-addr", "", "address to put in a Raft header")
	op := fs.String("op", "", "raft or status")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *addr == "" || *serverPin == "" || *keyDir == "" || *claimID == "" || (*op != "raft" && *op != "status") {
		return errors.New("rpc needs -addr, -server-pin, -key-dir, -claim-id and -op raft|status")
	}
	k, err := nodekey.Load(*keyDir)
	if err != nil {
		return err
	}
	fmt.Printf("%s RPC to %s with the key %s, claiming node %s\n", *op, *addr, k.Pin(), *claimID)
	if *op == "status" {
		var out map[string]any
		err := raftnet.Caller{NodeID: *claimID, Certificate: k.Certificate}.Call(context.Background(), *addr, *serverPin, raftnet.OpStatus, struct{}{}, &out)
		if err != nil {
			fmt.Printf("REFUSED: %v\n", err)
		} else {
			fmt.Printf("ACCEPTED: %v\n", out)
		}
		return nil
	}
	tc, err := raftnet.DialPinned(*addr, *serverPin, k.Certificate, 10*time.Second)
	if err != nil {
		fmt.Printf("REFUSED: %v\n", err)
		return nil
	}
	defer tc.Close()
	tc.SetDeadline(time.Now().Add(10 * time.Second))
	req := &raft.AppendEntriesRequest{RPCHeader: raft.RPCHeader{ProtocolVersion: 3, ID: []byte(*claimID), Addr: []byte(*claimAddr)}, Term: 1}
	w := bufio.NewWriter(tc)
	mp := &codec.MsgpackHandle{}
	mp.TimeNotBuiltin = true
	if err := w.WriteByte(0); err != nil { // AppendEntries
		return err
	}
	if err := codec.NewEncoder(w, mp).Encode(req); err != nil {
		return err
	}
	if err := w.Flush(); err != nil {
		return err
	}
	var respErr string
	if err := codec.NewDecoder(bufio.NewReader(tc), &codec.MsgpackHandle{}).Decode(&respErr); err != nil {
		fmt.Printf("REFUSED: the connection was closed without an answer (%v)\n", err)
		return nil
	}
	fmt.Printf("ACCEPTED: answered (error string %q)\n", respErr)
	return nil
}

func requestLine(args []string) error {
	fs := flag.NewFlagSet("request", flag.ContinueOnError)
	id := fs.String("id", "", "node ID (empty: a new one)")
	name := fs.String("name", "", "node name")
	address := fs.String("address", "", "ip:8444")
	pin := fs.String("pin", "", "key pin")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		var err error
		if *id, err = ids.NewUUID(); err != nil {
			return err
		}
	}
	line, err := joincode.EncodeRequest(joincode.Request{Node: joincode.Node{NodeID: *id, Name: *name, Address: *address, Pin: *pin}})
	if err != nil {
		return err
	}
	fmt.Println(line)
	return nil
}

type nodeList []joincode.Node

func (l *nodeList) String() string { return fmt.Sprint(*l) }

func (l *nodeList) Set(v string) error {
	n, err := parseNode(v)
	if err != nil {
		return err
	}
	*l = append(*l, n)
	return nil
}

func parseNode(v string) (joincode.Node, error) {
	f := strings.Split(v, ",")
	if len(f) != 4 {
		return joincode.Node{}, errors.New("a node is ID,NAME,ADDR,PIN")
	}
	return joincode.Node{NodeID: f[0], Name: f[1], Address: f[2], Pin: f[3]}, nil
}

func admissionLine(args []string) error {
	fs := flag.NewFlagSet("admission", flag.ContinueOnError)
	cid := fs.String("cluster-id", "", "cluster ID")
	forNode := fs.String("for", "", "ID,NAME,ADDR,PIN of the admitted node")
	var members nodeList
	fs.Var(&members, "member", "ID,NAME,ADDR,PIN of a member (repeat)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	f, err := parseNode(*forNode)
	if err != nil {
		return err
	}
	line, err := joincode.EncodeAdmission(joincode.Admission{ClusterID: *cid, For: f, Members: members, ExpiresAt: time.Now().Add(30 * time.Minute).Unix()})
	if err != nil {
		return err
	}
	fmt.Println(line)
	return nil
}
