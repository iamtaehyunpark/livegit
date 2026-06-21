package shell

import (
	"os"
	"path/filepath"

	"github.com/taehyun/lg/internal/config"
)

// hooksDir holds the generated shell-integration scripts.
func hooksDir() string { return filepath.Join(config.Dir(), "hooks") }

// ZshIntegration is the script sourced into the user's real zsh (D2, §5.1). It
// runs the user's zsh unchanged and only adds trigger detection:
//
//   - An accept-line ZLE widget inspects each command line BEFORE it runs and,
//     if it matches a SOURCE trigger, rewrites it to `lg enter-source ... -- <cmd>`
//     so the trigger command never executes locally — it runs on Source instead.
//   - A precmd hook keeps the [SOURCE]/[LOCAL] state reflected in the prompt.
//
// The actual trigger logic lives in Go (`lg hook check`) so zsh and the rest of
// the system share one implementation.
const ZshIntegration = `# lg (Live Git) zsh integration — auto-generated, do not edit.
# Sourced by 'lg shell'. Provides SOURCE-mode trigger detection via a ZLE widget.

_lg_check() {
  command lg hook check --tab "$LG_TAB_ID" --cwd "$PWD" -- "$1" 2>/dev/null
}

_lg_accept_line() {
  if [[ -n "$LG_TAB_ID" && -n "$BUFFER" ]]; then
    local out
    out="$(_lg_check "$BUFFER")"
    if [[ "$out" == ENTER* ]]; then
      local via="${out#ENTER }"
      BUFFER="lg enter-source --via ${via} -- ${BUFFER}"
    fi
  fi
  zle .accept-line
}
zle -N accept-line _lg_accept_line

_lg_precmd() {
  # Capture the user's real prompt once, then always rebuild from it so the tag
  # never stacks up.
  [[ -z "$LG_BASE_PS1" ]] && LG_BASE_PS1="$PS1"
  if command lg hook is-source --tab "$LG_TAB_ID" 2>/dev/null; then
    PS1="%K{208}%F{black} source/remote %f%k $LG_BASE_PS1"
  else
    PS1="%K{34}%F{black} ghost/local %f%k $LG_BASE_PS1"
  fi
}
typeset -ga precmd_functions
precmd_functions+=(_lg_precmd)
`

// BashIntegration is the best-effort bash variant (D2: bash's DEBUG trap fires
// redundantly inside subshells/functions, so this is not fully correct).
const BashIntegration = `# lg (Live Git) bash integration — auto-generated, best-effort (see D2).
_lg_debug() {
  [[ -z "$LG_TAB_ID" ]] && return
  # Only act on interactive top-level commands.
  [[ "$BASH_COMMAND" == _lg_* ]] && return
  local out
  out="$(command lg hook check --tab "$LG_TAB_ID" --cwd "$PWD" -- "$BASH_COMMAND" 2>/dev/null)"
  if [[ "$out" == ENTER* ]]; then
    local via="${out#ENTER }"
    lg enter-source --via "$via" -- "$BASH_COMMAND"
    return 1  # attempt to cancel the local command (best-effort)
  fi
}
trap '_lg_debug' DEBUG

# Prompt tag: show where commands run (ghost/local vs source/remote).
_lg_bash_prompt() {
  [ -z "$LG_BASE_PS1" ] && LG_BASE_PS1="$PS1"
  if command lg hook is-source --tab "$LG_TAB_ID" 2>/dev/null; then
    PS1="\[\e[48;5;208m\e[30m\] source/remote \[\e[0m\] $LG_BASE_PS1"
  else
    PS1="\[\e[48;5;34m\e[30m\] ghost/local \[\e[0m\] $LG_BASE_PS1"
  fi
}
case "$PROMPT_COMMAND" in
  *_lg_bash_prompt*) ;;
  *) PROMPT_COMMAND="_lg_bash_prompt${PROMPT_COMMAND:+; $PROMPT_COMMAND}" ;;
esac
`

// InstallIntegration writes the integration scripts to ~/.lg/hooks and returns
// the path to source for the given shell.
func InstallIntegration() (zshPath, bashPath string, err error) {
	if err := os.MkdirAll(hooksDir(), 0o755); err != nil {
		return "", "", err
	}
	zshPath = filepath.Join(hooksDir(), "zsh-integration.zsh")
	bashPath = filepath.Join(hooksDir(), "bash-integration.bash")
	if err := os.WriteFile(zshPath, []byte(ZshIntegration), 0o644); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(bashPath, []byte(BashIntegration), 0o644); err != nil {
		return "", "", err
	}
	return zshPath, bashPath, nil
}
