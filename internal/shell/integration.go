package shell

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/iamtaehyunpark/livegit/internal/config"
	"github.com/iamtaehyunpark/livegit/internal/shellq"
)

// hooksDir holds the generated shell-integration scripts.
func hooksDir() string { return filepath.Join(config.Dir(), "hooks") }

// zshIntegration is sourced into the user's real zsh by `lg shell`. It runs the
// user's zsh unchanged and adds two things, both keyed on the first word of the
// command line:
//   - toggle mode: when on, EVERY command is rewritten to `lg run`.
//   - auto-remote commands: a fixed list (ls/cat/tree/…) that always run on
//     Source even with toggle off, falling back to the local command if Source
//     is unreachable (`lg run --local-fallback`).
//
// There is no auto-detection beyond this explicit first-word list.
func zshIntegration(autoCmds []string) string {
	return `# lg (Live Git) zsh integration — auto-generated, do not edit.
# Sourced by 'lg shell'.

_lg_auto=(` + zshList(autoCmds) + `)

_lg_toggled() {
  command lg hook is-toggled --tab "$LG_TAB_ID" 2>/dev/null
}

# auto-route applies only inside the mounted folder, so commands elsewhere stay
# normal local commands.
_lg_in_mount() {
  [[ -n "$LG_LOCAL_ROOT" && ( "$PWD/" == "$LG_LOCAL_ROOT/"* ) ]]
}

# _lg_first <line> -> the command's first word, path stripped (e.g. /bin/ls -> ls)
_lg_first() {
  local w=${1%%[[:space:]]*}
  print -r -- ${w##*/}
}

_lg_accept_line() {
  if [[ -n "$LG_TAB_ID" && -n "$BUFFER" ]]; then
    if _lg_toggled; then
      BUFFER="lg run -- ${BUFFER}"
    elif _lg_in_mount && (( ${_lg_auto[(Ie)$(_lg_first "$BUFFER")]} )); then
      BUFFER="lg run --local-fallback -- ${BUFFER}"
    fi
  fi
  zle .accept-line
}
zle -N accept-line _lg_accept_line

_lg_precmd() {
  [[ -z "$LG_BASE_PS1" ]] && LG_BASE_PS1="$PS1"
  if _lg_toggled; then
    PS1="%K{208}%F{black} remote %f%k $LG_BASE_PS1"
  else
    PS1="$LG_BASE_PS1"
  fi
}
typeset -ga precmd_functions
precmd_functions+=(_lg_precmd)
`
}

// bashIntegration is the best-effort bash variant (bash's DEBUG trap fires
// redundantly inside subshells/functions, so this is not fully correct).
func bashIntegration(autoCmds []string) string {
	return `# lg (Live Git) bash integration — auto-generated, best-effort.
_lg_auto=" ` + strings.Join(autoCmds, " ") + ` "

_lg_debug() {
  [[ -z "$LG_TAB_ID" ]] && return
  [[ "$BASH_COMMAND" == _lg_* ]] && return
  [[ "$BASH_COMMAND" == lg\ run* ]] && return
  local w=${BASH_COMMAND%%[[:space:]]*}; w=${w##*/}
  if command lg hook is-toggled --tab "$LG_TAB_ID" 2>/dev/null; then
    lg run -- "$BASH_COMMAND"; return 1
  elif [[ -n "$LG_LOCAL_ROOT" && "$PWD/" == "$LG_LOCAL_ROOT/"* && "$_lg_auto" == *" $w "* ]]; then
    lg run --local-fallback -- "$BASH_COMMAND"; return 1
  fi
}
trap '_lg_debug' DEBUG

_lg_bash_prompt() {
  [ -z "$LG_BASE_PS1" ] && LG_BASE_PS1="$PS1"
  if command lg hook is-toggled --tab "$LG_TAB_ID" 2>/dev/null; then
    PS1="\[\e[48;5;208m\e[30m\] remote \[\e[0m\] $LG_BASE_PS1"
  else
    PS1="$LG_BASE_PS1"
  fi
}
case "$PROMPT_COMMAND" in
  *_lg_bash_prompt*) ;;
  *) PROMPT_COMMAND="_lg_bash_prompt${PROMPT_COMMAND:+; $PROMPT_COMMAND}" ;;
esac
`
}

// zshList renders a quoted zsh array body from the command list.
func zshList(cmds []string) string {
	out := make([]string, len(cmds))
	for i, c := range cmds {
		out[i] = shellq.Quote(c)
	}
	return strings.Join(out, " ")
}

// InstallIntegration writes the integration scripts (with the auto-remote
// command list baked in) to ~/.lg/hooks and returns their paths.
func InstallIntegration(autoCmds []string) (zshPath, bashPath string, err error) {
	if err := os.MkdirAll(hooksDir(), 0o755); err != nil {
		return "", "", err
	}
	zshPath = filepath.Join(hooksDir(), "zsh-integration.zsh")
	bashPath = filepath.Join(hooksDir(), "bash-integration.bash")
	if err := os.WriteFile(zshPath, []byte(zshIntegration(autoCmds)), 0o644); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(bashPath, []byte(bashIntegration(autoCmds)), 0o644); err != nil {
		return "", "", err
	}
	return zshPath, bashPath, nil
}
