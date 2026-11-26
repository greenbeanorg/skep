// Command 8c2-joinreq prints a well-formed join request line for a made-up
// host (name wu, node ID and pin fixed, TEST-NET address), for the step
// 8c-2 lab's POST /api/v1/cluster/admissions on a host without a listen
// address. No host with that key exists.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/greenbeanorg/skep/internal/cluster/joincode"
)

func main() {
	line, err := joincode.EncodeRequest(joincode.Request{Node: joincode.Node{NodeID: "2d7a5c74-9b2a-4e7a-9f2d-5b3f8b2c1e33",
		Name: "wu", Address: "192.0.2.9:8444", Pin: "sha256:" + strings.Repeat("ab", 32)}})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(line)
}
