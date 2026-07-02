// Package shellq is the single, audited implementation of POSIX shell quoting.
//
// lg builds shell command lines in several places — the remote `lg serve`
// invocation, the detached-job wrapper script, the generated shell hooks, and
// the command runner that reconstructs a command line from argv. Quoting is
// security-sensitive (a missed metacharacter is a command-injection bug), so it
// lives here once rather than being re-derived per package.
package shellq

import "strings"

// Quote wraps s in single quotes so the shell treats it as one literal word,
// escaping any embedded single quotes. Single quotes are the only fully robust
// POSIX mechanism: inside them every character is literal, so no metacharacter
// ($, `, *, ;, whitespace, …) can be re-interpreted.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Join renders argv as a single shell command line, quoting every token so each
// element survives re-parsing by the remote `sh -lc` exactly as passed. Quoting
// unconditionally (not just tokens with whitespace) is what keeps an argument
// like "a;b" or "$HOME" a literal argument instead of shell syntax.
func Join(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = Quote(a)
	}
	return strings.Join(parts, " ")
}
