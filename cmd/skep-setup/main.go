// Command skep-setup configures a FreeBSD host for Skep after the package
// is installed (ADR 0012).
package main

import (
	"fmt"
	"os"

	"github.com/greenbeanorg/skep/pkg/version"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-version" || os.Args[1] == "version") {
		fmt.Println("skep-setup", version.Get())
		return
	}
	fmt.Fprintln(os.Stderr, "skep-setup: not yet implemented")
	os.Exit(2)
}
