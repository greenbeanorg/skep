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
//
// It reads no Skep file and changes nothing.
package main

import (
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
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: cluster-probe get|tls ...")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "get":
		err = get(os.Args[2:])
	case "tls":
		err = tlsProbe(os.Args[2:])
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
