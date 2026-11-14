package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildSessionStartOutput(t *testing.T) {
	// 1. Non-lg directory produces zero output.
	nonLG := t.TempDir()
	out, err := buildSessionStartOutput([]byte(`{"cwd":"`+nonLG+`"}`), "auto", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "" {
		t.Fatalf("expected empty output outside lg project, got %q", out)
	}

	// 2. lg project via cwd -> plain text output.
	proj := t.TempDir()
	lgDir := filepath.Join(proj, ".lg")
	if err := os.MkdirAll(lgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgYAML := "role: ghost\nsource:\n  host: gpu-box\n  remote_root: /home/u/repo\n"
	if err := os.WriteFile(filepath.Join(lgDir, "config.yaml"), []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	sub := filepath.Join(proj, "nested", "dir")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	textOut, err := buildSessionStartOutput([]byte(`{"cwd":"`+sub+`"}`), "auto", "")
	if err != nil {
		t.Fatalf("text session-start error: %v", err)
	}
	if !strings.Contains(textOut, "[livegit]") || !strings.Contains(textOut, "gpu-box:/home/u/repo") {
		t.Fatalf("unexpected text output: %q", textOut)
	}

	// 3. lg project via workspacePaths (Antigravity) -> JSON injectSteps.
	agyIn := `{"hookEventName":"SessionStart","workspacePaths":["` + proj + `"]}`
	agyOut, err := buildSessionStartOutput([]byte(agyIn), "auto", "")
	if err != nil {
		t.Fatalf("antigravity session-start error: %v", err)
	}
	var parsed struct {
		InjectSteps []struct {
			SystemMessage struct {
				SystemMessage string `json:"systemMessage"`
			} `json:"systemMessage"`
		} `json:"injectSteps"`
	}
	if err := json.Unmarshal([]byte(agyOut), &parsed); err != nil {
		t.Fatalf("invalid antigravity JSON %q: %v", agyOut, err)
	}
	if len(parsed.InjectSteps) != 1 || !strings.Contains(parsed.InjectSteps[0].SystemMessage.SystemMessage, "[livegit]") {
		t.Fatalf("unexpected injectSteps: %+v", parsed.InjectSteps)
	}
}

func TestInstallAndUninstallHooks(t *testing.T) {
	home := t.TempDir()

	// Pre-populate existing user settings in Claude, Codex, and Antigravity.
	claudePath := filepath.Join(home, ".claude", "settings.json")
	if err := writeJSONMap(claudePath, map[string]any{
		"theme": "dark",
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{
					"matcher": "startup",
					"hooks":   []any{map[string]any{"type": "command", "command": "echo other"}},
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	codexPath := filepath.Join(home, ".codex", "hooks.json")
	if err := writeJSONMap(codexPath, map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{"type": "command", "command": "echo codex-other"},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	agyPath := filepath.Join(home, ".gemini", "config", "hooks.json")
	if err := writeJSONMap(agyPath, map[string]any{
		"other-hook": map[string]any{"PreInvocation": []any{}},
	}); err != nil {
		t.Fatal(err)
	}

	// Install twice to verify idempotency.
	for i := 0; i < 2; i++ {
		if err := installHooksAt("all", home); err != nil {
			t.Fatalf("installHooksAt(%d): %v", i, err)
		}
	}

	// Claude: 2 entries (other + livegit), theme preserved.
	cm, _ := readJSONMap(claudePath)
	if cm["theme"] != "dark" {
		t.Fatalf("claude theme lost: %v", cm)
	}
	cHooks := sliceOfAny(cm["hooks"].(map[string]any)["SessionStart"])
	if len(cHooks) != 2 {
		t.Fatalf("claude SessionStart len = %d, want 2: %v", len(cHooks), cHooks)
	}

	// Codex: 2 entries (other + livegit).
	xm, _ := readJSONMap(codexPath)
	xHooks := sliceOfAny(xm["hooks"].(map[string]any)["SessionStart"])
	if len(xHooks) != 2 {
		t.Fatalf("codex SessionStart len = %d, want 2: %v", len(xHooks), xHooks)
	}

	// Antigravity: other-hook + livegit.
	am, _ := readJSONMap(agyPath)
	if _, ok := am["other-hook"]; !ok {
		t.Fatalf("antigravity other-hook lost: %v", am)
	}
	if _, ok := am["livegit"]; !ok {
		t.Fatalf("antigravity livegit hook missing: %v", am)
	}

	// Uninstall: removes only livegit entries, keeps existing ones.
	if err := uninstallHooksAt("all", home); err != nil {
		t.Fatalf("uninstallHooksAt: %v", err)
	}
	cm, _ = readJSONMap(claudePath)
	if got := len(sliceOfAny(cm["hooks"].(map[string]any)["SessionStart"])); got != 1 {
		t.Fatalf("claude after uninstall len = %d, want 1", got)
	}
	xm, _ = readJSONMap(codexPath)
	if got := len(sliceOfAny(xm["hooks"].(map[string]any)["SessionStart"])); got != 1 {
		t.Fatalf("codex after uninstall len = %d, want 1", got)
	}
	am, _ = readJSONMap(agyPath)
	if _, ok := am["livegit"]; ok {
		t.Fatalf("antigravity livegit still present after uninstall")
	}
	if _, ok := am["other-hook"]; !ok {
		t.Fatalf("antigravity other-hook lost after uninstall")
	}
}

func TestInstallAndUninstallSkill(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := installSkillAt("auto", home); err != nil {
		t.Fatalf("installSkillAt: %v", err)
	}

	for _, base := range []string{
		filepath.Join(home, ".agents", "skills", "livegit"),
		filepath.Join(home, ".claude", "skills", "livegit"),
	} {
		if _, err := os.Stat(filepath.Join(base, "SKILL.md")); err != nil {
			t.Errorf("missing SKILL.md at %s: %v", base, err)
		}
		if _, err := os.Stat(filepath.Join(base, "references", "troubleshooting.md")); err != nil {
			t.Errorf("missing troubleshooting.md at %s: %v", base, err)
		}
	}

	// Codex dir didn't exist so auto should not have created it.
	if _, err := os.Stat(filepath.Join(home, ".codex", "skills", "livegit")); !os.IsNotExist(err) {
		t.Errorf("unexpected codex skill dir created in auto mode")
	}

	if err := uninstallSkillAt("all", home); err != nil {
		t.Fatalf("uninstallSkillAt: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".agents", "skills", "livegit")); !os.IsNotExist(err) {
		t.Errorf("skill dir still exists after uninstall")
	}
}
