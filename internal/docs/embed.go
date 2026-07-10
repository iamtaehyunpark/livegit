// Package docs embeds the user- and agent-facing guides so `lg init` can drop
// them into the project root. That way any CLI or coding agent working in an lg
// project has GUIDE.md (the human guide) and AGENTS.md (the agent harness) right
// next to the code it's driving, with no network fetch. The agent guide is also
// dropped as CLAUDE.md — same content — because Claude Code reads CLAUDE.md
// natively but does not pick up AGENTS.md on its own.
//
// The copies here are kept in sync with the repo-root originals by the Makefile
// `docs` target (run as part of `make build`); they are committed so plain
// `go build`/`go test` embed them too.
package docs

import _ "embed"

//go:embed GUIDE.md
var Guide []byte

//go:embed AGENTS.md
var Agents []byte

// File pairs a project-root filename with its embedded contents.
type File struct {
	Name    string
	Content []byte
}

// Files are the docs `lg init` writes into the project root. CLAUDE.md is the
// AGENTS.md content under the filename Claude Code actually loads; both stay
// marker-gated, so a project's own CLAUDE.md (no lg marker) is never touched.
func Files() []File {
	return []File{
		{Name: "GUIDE.md", Content: Guide},
		{Name: "AGENTS.md", Content: Agents},
		{Name: "CLAUDE.md", Content: Agents},
	}
}
