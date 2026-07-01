// Package docs embeds the user- and agent-facing guides so `lg init` can drop
// them into the project root. That way any CLI or coding agent working in an lg
// project has GUIDE.md (the human guide) and AGENTS.md (the agent harness) right
// next to the code it's driving, with no network fetch.
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

// Files are the docs `lg init` writes into the project root.
func Files() []File {
	return []File{
		{Name: "GUIDE.md", Content: Guide},
		{Name: "AGENTS.md", Content: Agents},
	}
}
