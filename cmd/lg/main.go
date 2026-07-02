// Command lg is the single Live Git binary. Its role (Ghost or Source) is
// determined by `lg init --role` and the subcommand invoked.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/iamtaehyunpark/livegit/internal/cli"
)

func main() {
	root := cli.NewRoot()

	// Bare-command passthrough: `lg <anything not a subcommand>` runs that
	// command on Source. Only intercept when the first arg is a plain word — not a
	// flag (-h/--help/--version) and not a known subcommand — so the normal CLI
	// surface keeps working.
	if args := os.Args[1:]; len(args) > 0 {
		first := args[0]
		if !strings.HasPrefix(first, "-") && !cli.IsKnownSubcommand(root, first) {
			os.Exit(cli.RunPassthrough(args))
		}
	}

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "lg:", err)
		os.Exit(1)
	}
}
