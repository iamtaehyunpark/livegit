package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/iamtaehyunpark/livegit/internal/docs"
)

// Project-root docs (GUIDE.md / AGENTS.md) are managed like the remote agent
// binary: dropped by `lg init`, then kept in step with the running build by
// `lg connect` / `lg refresh` when the version changes. A marker line at the
// top records which version wrote the file AND gates the refresh — a file
// without the marker (it existed before lg, or its owner deleted the line to
// take ownership) is never touched. Files written before markers existed
// (≤ v1.2.1) count as user-owned too; delete them once to adopt.

const docMarker = "<!-- lg:doc "

func docStamp(version string) string {
	return docMarker + version +
		" · managed by lg — refreshed automatically on upgrade; remove this line to keep your own copy -->\n\n"
}

// docVersion extracts the version from a stamped doc ("" when unmarked).
func docVersion(b []byte) string {
	if !bytes.HasPrefix(b, []byte(docMarker)) {
		return ""
	}
	rest := b[len(docMarker):]
	if i := bytes.IndexAny(rest, " \n"); i > 0 {
		return string(rest[:i])
	}
	return ""
}

// syncDocFile writes or refreshes one doc at dst and reports what happened:
// "written" (was missing), "refreshed" (marker version differed), or ""
// (left alone — up to date, user-owned, or unwritable).
func syncDocFile(dst string, content []byte, version string) string {
	existing, err := os.ReadFile(dst)
	if err != nil && !os.IsNotExist(err) {
		return ""
	}
	stamped := append([]byte(docStamp(version)), content...)
	if err != nil { // missing
		if os.WriteFile(dst, stamped, 0o644) != nil {
			return ""
		}
		return "written"
	}
	have := docVersion(existing)
	if have == "" {
		return "" // no marker: user-owned, never touched
	}
	// A "dev" build carries no comparable version — never churn stamped files.
	if have == version || version == "dev" {
		return ""
	}
	if os.WriteFile(dst, stamped, 0o644) != nil {
		return ""
	}
	return "refreshed"
}

// syncProjectDocs keeps the guides in projectRoot current with this binary.
// Purely local and best-effort; called by init, connect, and refresh.
func syncProjectDocs(projectRoot string) {
	for _, d := range docs.Files() {
		dst := filepath.Join(projectRoot, d.Name)
		switch syncDocFile(dst, d.Content, Version) {
		case "written":
			fmt.Printf("✓ wrote %s\n", dst)
		case "refreshed":
			fmt.Printf("✓ refreshed %s (docs from lg %s)\n", d.Name, Version)
		}
	}
}
