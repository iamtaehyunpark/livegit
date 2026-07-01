// Package agentbin embeds the Linux `lg` agent binaries so `lg init` can deploy
// the agent to a Source that doesn't have it yet. The binaries are built from
// the same source at build time (see the Makefile `agents` target), so the
// deployed agent always matches this binary's protocol version.
//
// In a plain `go build`/`go test` (without `make`), data/ holds only .gitkeep,
// so Pick returns nil and the deploy path degrades to printing a manual command.
package agentbin

import "embed"

//go:embed all:data
var data embed.FS

// Pick returns the embedded Linux agent for a GOARCH ("amd64"/"arm64"), or nil
// if it isn't embedded in this build.
func Pick(goarch string) []byte {
	b, err := data.ReadFile("data/lg-linux-" + goarch)
	if err != nil || len(b) == 0 {
		return nil
	}
	return b
}
