package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/iamtaehyunpark/livegit/internal/config"
	"github.com/iamtaehyunpark/livegit/internal/fuse"
	"github.com/iamtaehyunpark/livegit/internal/shell"
	"github.com/iamtaehyunpark/livegit/internal/transport"
	"github.com/spf13/cobra"
)

// newHookCmd groups shell-integration callbacks and agent lifecycle hooks.
func newHookCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hook",
		Short: "Manage agent session-start hooks and shell callbacks",
	}
	cmd.AddCommand(
		newHookIsToggledCmd(),
		newHookSessionStartCmd(),
		newHookInstallCmd(),
		newHookUninstallCmd(),
	)
	return cmd
}

func newHookIsToggledCmd() *cobra.Command {
	var tab string
	c := &cobra.Command{
		Use:    "is-toggled",
		Short:  "exit 0 if the tab has toggle mode on",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if shell.ToggleOn(tab) {
				return nil
			}
			os.Exit(1)
			return nil
		},
	}
	c.Flags().StringVar(&tab, "tab", "", "terminal tab id")
	return c
}

type sessionStartInput struct {
	Cwd            string   `json:"cwd"`
	WorkspacePaths []string `json:"workspacePaths"`
	HookEventName  string   `json:"hookEventName"`
}

func newHookSessionStartCmd() *cobra.Command {
	var format string
	c := &cobra.Command{
		Use:   "session-start",
		Short: "Emit livegit project context if cwd is an lg project (used by agent hooks)",
		RunE: func(cmd *cobra.Command, args []string) error {
			var raw []byte
			if st, err := os.Stdin.Stat(); err == nil && (st.Mode()&os.ModeCharDevice) == 0 {
				raw, _ = io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
			}
			out, err := buildSessionStartOutput(raw, format, "")
			if err != nil {
				return err
			}
			if out != "" {
				fmt.Fprint(cmd.OutOrStdout(), out)
			}
			return nil
		},
	}
	c.Flags().StringVar(&format, "format", "auto", "output format: auto|text|antigravity")
	return c
}

// buildSessionStartOutput inspects the hook stdin payload (or fallbackDir/cwd)
// and returns the context string to inject, or "" when not in an lg project.
func buildSessionStartOutput(stdin []byte, format, fallbackDir string) (string, error) {
	var in sessionStartInput
	if len(strings.TrimSpace(string(stdin))) > 0 {
		_ = json.Unmarshal(stdin, &in)
	}

	startDir := in.Cwd
	if startDir == "" && len(in.WorkspacePaths) > 0 {
		startDir = in.WorkspacePaths[0]
	}
	if startDir == "" {
		startDir = fallbackDir
	}
	if startDir == "" {
		var err error
		startDir, err = os.Getwd()
		if err != nil {
			return "", nil
		}
	}

	lgDir, ok := config.FindProjectDir(startDir)
	if !ok {
		return "", nil
	}
	c, err := config.LoadFrom(filepath.Join(lgDir, "config.yaml"))
	if err != nil {
		return "", nil
	}

	msg := formatProjectHookMessage(c, lgDir)
	resolvedFmt := format
	if resolvedFmt == "" || resolvedFmt == "auto" {
		if len(in.WorkspacePaths) > 0 || in.HookEventName != "" {
			resolvedFmt = "antigravity"
		} else {
			resolvedFmt = "text"
		}
	}

	switch resolvedFmt {
	case "antigravity":
		payload := map[string]any{
			"injectSteps": []map[string]any{
				{
					"systemMessage": map[string]string{
						"systemMessage": msg,
					},
				},
			},
		}
		b, err := json.Marshal(payload)
		if err != nil {
			return "", err
		}
		return string(b) + "\n", nil
	case "text":
		return msg + "\n", nil
	default:
		return "", fmt.Errorf("unknown --format %q (want auto|text|antigravity)", format)
	}
}

func formatProjectHookMessage(c *config.Config, lgDir string) string {
	if c.Role == config.RoleSource {
		return "[livegit] This workspace is a livegit source repository."
	}

	mp := c.MountDirFrom(lgDir)
	mountState := "not mounted — run 'lg mount' before editing files"
	if fuse.IsMounted(mp) {
		mountState = "mounted"
	}

	var connState string
	switch {
	case c.Source.SSHMode == "native":
		connState = "per-command (native ssh)"
	case transport.MasterLive(c):
		connState = "live"
	default:
		connState = "down — ask the user to run 'lg connect'"
	}

	return fmt.Sprintf(
		"[livegit] This workspace is an lg project (remote: %s:%s, mount: %s [%s], connection: %s). "+
			"Use the 'livegit' skill (or run 'lg skill show') before running remote commands ('lg run -- <cmd>') or editing files.",
		c.Source.Host, c.Source.RemoteRoot, mp, mountState, connState,
	)
}

func newHookInstallCmd() *cobra.Command {
	var target string
	c := &cobra.Command{
		Use:   "install",
		Short: "Install the livegit SessionStart hook for Claude Code, Codex, and Antigravity",
		RunE: func(cmd *cobra.Command, args []string) error {
			return installHooks(target)
		},
	}
	c.Flags().StringVar(&target, "target", "auto", "target tool(s): auto|all|claude|codex|antigravity")
	return c
}

func newHookUninstallCmd() *cobra.Command {
	var target string
	c := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the livegit SessionStart hook from agent configs",
		RunE: func(cmd *cobra.Command, args []string) error {
			return uninstallHooks(target)
		},
	}
	c.Flags().StringVar(&target, "target", "all", "target tool(s): all|claude|codex|antigravity")
	return c
}

func lgHookCommand(extraArgs ...string) string {
	bin := "lg"
	if _, err := exec.LookPath("lg"); err != nil {
		if exe, exeErr := os.Executable(); exeErr == nil {
			bin = exe
		}
	}
	parts := append([]string{bin, "hook", "session-start"}, extraArgs...)
	return strings.Join(parts, " ")
}

func resolveHookTargets(target string, home string, createMissing bool) ([]string, error) {
	type candidate struct {
		name string
		dir  string
	}
	all := []candidate{
		{"claude", filepath.Join(home, ".claude")},
		{"codex", filepath.Join(home, ".codex")},
		{"antigravity", filepath.Join(home, ".gemini")},
	}
	switch target {
	case "claude", "codex", "antigravity":
		return []string{target}, nil
	case "all":
		return []string{"claude", "codex", "antigravity"}, nil
	case "auto":
		var out []string
		for _, c := range all {
			if st, err := os.Stat(c.dir); err == nil && st.IsDir() {
				out = append(out, c.name)
			}
		}
		if len(out) == 0 && createMissing {
			out = []string{"claude", "codex", "antigravity"}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unknown --target %q (want auto|all|claude|codex|antigravity)", target)
	}
}

func installHooks(target string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	return installHooksAt(target, home)
}

func installHooksAt(target, home string) error {
	targets, err := resolveHookTargets(target, home, true)
	if err != nil {
		return err
	}
	for _, t := range targets {
		switch t {
		case "claude":
			p := filepath.Join(home, ".claude", "settings.json")
			if err := installClaudeHook(p, lgHookCommand()); err != nil {
				return err
			}
			fmt.Printf("✓ installed Claude Code SessionStart hook in %s\n", p)
		case "codex":
			p := filepath.Join(home, ".codex", "hooks.json")
			if err := installCodexHook(p, lgHookCommand()); err != nil {
				return err
			}
			fmt.Printf("✓ installed Codex SessionStart hook in %s (run /hooks in Codex once to trust)\n", p)
		case "antigravity":
			p := filepath.Join(home, ".gemini", "config", "hooks.json")
			if err := installAntigravityHook(p, lgHookCommand("--format", "antigravity")); err != nil {
				return err
			}
			fmt.Printf("✓ installed Antigravity SessionStart hook in %s\n", p)
		}
	}
	return nil
}

func uninstallHooks(target string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	return uninstallHooksAt(target, home)
}

func uninstallHooksAt(target, home string) error {
	targets, err := resolveHookTargets(target, home, false)
	if err != nil {
		return err
	}
	for _, t := range targets {
		switch t {
		case "claude":
			p := filepath.Join(home, ".claude", "settings.json")
			if removed, err := removeClaudeHook(p); err != nil {
				return err
			} else if removed {
				fmt.Printf("✓ removed Claude Code SessionStart hook from %s\n", p)
			}
		case "codex":
			p := filepath.Join(home, ".codex", "hooks.json")
			if removed, err := removeCodexHook(p); err != nil {
				return err
			} else if removed {
				fmt.Printf("✓ removed Codex SessionStart hook from %s\n", p)
			}
		case "antigravity":
			p := filepath.Join(home, ".gemini", "config", "hooks.json")
			if removed, err := removeAntigravityHook(p); err != nil {
				return err
			} else if removed {
				fmt.Printf("✓ removed Antigravity SessionStart hook from %s\n", p)
			}
		}
	}
	return nil
}

func readJSONMap(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) || len(strings.TrimSpace(string(b))) == 0 {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func writeJSONMap(path string, m map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func isLivegitHookCmd(cmd string) bool {
	return strings.Contains(cmd, "hook session-start")
}

func ensureSubMap(parent map[string]any, key string) map[string]any {
	if v, ok := parent[key].(map[string]any); ok {
		return v
	}
	m := map[string]any{}
	parent[key] = m
	return m
}

func sliceOfAny(v any) []any {
	if s, ok := v.([]any); ok {
		return s
	}
	return nil
}

func installClaudeHook(path, command string) error {
	root, err := readJSONMap(path)
	if err != nil {
		return err
	}
	hooks := ensureSubMap(root, "hooks")
	existing := sliceOfAny(hooks["SessionStart"])

	filtered := make([]any, 0, len(existing)+1)
	for _, item := range existing {
		if claudeEntryHasLivegit(item) {
			continue
		}
		filtered = append(filtered, item)
	}
	filtered = append(filtered, map[string]any{
		"matcher": "startup",
		"hooks": []any{
			map[string]any{
				"type":          "command",
				"command":       command,
				"statusMessage": "Checking livegit project...",
			},
		},
	})
	hooks["SessionStart"] = filtered
	return writeJSONMap(path, root)
}

func removeClaudeHook(path string) (bool, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false, nil
	}
	root, err := readJSONMap(path)
	if err != nil {
		return false, err
	}
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		return false, nil
	}
	existing := sliceOfAny(hooks["SessionStart"])
	filtered := make([]any, 0, len(existing))
	removed := false
	for _, item := range existing {
		if claudeEntryHasLivegit(item) {
			removed = true
			continue
		}
		filtered = append(filtered, item)
	}
	if !removed {
		return false, nil
	}
	if len(filtered) == 0 {
		delete(hooks, "SessionStart")
	} else {
		hooks["SessionStart"] = filtered
	}
	return true, writeJSONMap(path, root)
}

func claudeEntryHasLivegit(item any) bool {
	m, ok := item.(map[string]any)
	if !ok {
		return false
	}
	for _, h := range sliceOfAny(m["hooks"]) {
		if hm, ok := h.(map[string]any); ok {
			if cmd, _ := hm["command"].(string); isLivegitHookCmd(cmd) {
				return true
			}
		}
	}
	return false
}

func installCodexHook(path, command string) error {
	root, err := readJSONMap(path)
	if err != nil {
		return err
	}
	hooks := ensureSubMap(root, "hooks")
	existing := sliceOfAny(hooks["SessionStart"])
	filtered := make([]any, 0, len(existing)+1)
	for _, item := range existing {
		if hm, ok := item.(map[string]any); ok {
			if cmd, _ := hm["command"].(string); isLivegitHookCmd(cmd) {
				continue
			}
		}
		filtered = append(filtered, item)
	}
	filtered = append(filtered, map[string]any{
		"type":    "command",
		"command": command,
	})
	hooks["SessionStart"] = filtered
	return writeJSONMap(path, root)
}

func removeCodexHook(path string) (bool, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false, nil
	}
	root, err := readJSONMap(path)
	if err != nil {
		return false, err
	}
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		return false, nil
	}
	existing := sliceOfAny(hooks["SessionStart"])
	filtered := make([]any, 0, len(existing))
	removed := false
	for _, item := range existing {
		if hm, ok := item.(map[string]any); ok {
			if cmd, _ := hm["command"].(string); isLivegitHookCmd(cmd) {
				removed = true
				continue
			}
		}
		filtered = append(filtered, item)
	}
	if !removed {
		return false, nil
	}
	if len(filtered) == 0 {
		delete(hooks, "SessionStart")
	} else {
		hooks["SessionStart"] = filtered
	}
	return true, writeJSONMap(path, root)
}

func installAntigravityHook(path, command string) error {
	root, err := readJSONMap(path)
	if err != nil {
		return err
	}
	root["livegit"] = map[string]any{
		"SessionStart": []any{
			map[string]any{
				"type":    "command",
				"command": command,
			},
		},
	}
	return writeJSONMap(path, root)
}

func removeAntigravityHook(path string) (bool, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false, nil
	}
	root, err := readJSONMap(path)
	if err != nil {
		return false, err
	}
	if _, ok := root["livegit"]; !ok {
		return false, nil
	}
	delete(root, "livegit")
	return true, writeJSONMap(path, root)
}
