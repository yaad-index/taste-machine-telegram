// Command taste-machine-telegram is a Telegram frontend for the
// taste-machine engine.
package main

import (
	"fmt"
	"io"
	"os"
)

// version is set at release build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "version" || args[0] == "-version" || args[0] == "--version") {
		_, _ = fmt.Fprintln(stdout, "taste-machine-telegram", version)
		return 0
	}
	_, _ = fmt.Fprintln(stderr, "usage: taste-machine-telegram version")
	return 2
}
