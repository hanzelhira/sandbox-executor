// Command dumpprofile writes the seccomp profile to stdout, for inspection
// and for testing it directly with `docker run --security-opt seccomp=...`.
package main

import (
	"fmt"
	"os"

	"github.com/hanzelhira/sandbox-executor/internal/executor"
)

func main() {
	p, n, err := executor.SeccompProfileJSON()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "allowlist: %d syscalls\n", n)
	fmt.Print(p)
}
