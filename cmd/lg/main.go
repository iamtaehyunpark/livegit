// Command lg is the single Live Git binary. Its role (Ghost or Source) is
// determined by `lg init --role` and the subcommand invoked.
package main

import (
	"fmt"
	"os"

	"github.com/taehyun/lg/internal/cli"
)

func main() {
	if err := cli.NewRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "lg:", err)
		os.Exit(1)
	}
}
