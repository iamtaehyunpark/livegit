// Package docs embeds the human-facing guide and the agent-facing `livegit`
// skill so `lg` can drop GUIDE.md into project roots and install the skill
// globally via `lg skill install` with no network fetch.
//
// The GUIDE.md copy here is kept in sync with the repo-root original by the
// Makefile `docs` target (run as part of `make build`); it is committed so
// plain `go build`/`go test` embed it too.
package docs

import _ "embed"

//go:embed GUIDE.md
var Guide []byte

//go:embed skill/SKILL.md
var SkillMD []byte

//go:embed skill/references/troubleshooting.md
var SkillTroubleshooting []byte

// File pairs a relative filename with its embedded contents.
type File struct {
	Name    string
	Content []byte
}

// Files are the docs `lg init` / `lg connect` / `lg refresh` write into the
// project root. Only GUIDE.md (the human guide) is placed per-project; agent
// instructions live in the `livegit` skill + session-start hook instead.
func Files() []File {
	return []File{
		{Name: "GUIDE.md", Content: Guide},
	}
}

// SkillFiles returns the files that make up the `livegit` agent skill,
// relative to the skill directory (`livegit/`).
func SkillFiles() []File {
	return []File{
		{Name: "SKILL.md", Content: SkillMD},
		{Name: "references/troubleshooting.md", Content: SkillTroubleshooting},
	}
}
