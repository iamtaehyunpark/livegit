package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/iamtaehyunpark/livegit/internal/docs"
	"github.com/spf13/cobra"
)

func newSkillCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "Install or inspect the livegit agent skill",
	}
	cmd.AddCommand(
		newSkillInstallCmd(),
		newSkillUninstallCmd(),
		newSkillShowCmd(),
	)
	return cmd
}

func newSkillInstallCmd() *cobra.Command {
	var target string
	var withHook bool
	c := &cobra.Command{
		Use:   "install",
		Short: "Install the livegit skill (and SessionStart hook) for coding agents",
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			if err := installSkillAt(target, home); err != nil {
				return err
			}
			if withHook {
				hookTarget := target
				if hookTarget == "agents" {
					hookTarget = "auto"
				}
				if err := installHooksAt(hookTarget, home); err != nil {
					return err
				}
			}
			return nil
		},
	}
	c.Flags().StringVar(&target, "target", "auto", "target tool(s): auto|all|agents|claude|codex|antigravity")
	c.Flags().BoolVar(&withHook, "with-hook", true, "also install the SessionStart hook for automatic .lg/ detection")
	return c
}

func newSkillUninstallCmd() *cobra.Command {
	var target string
	var withHook bool
	c := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the livegit skill (and optionally SessionStart hooks)",
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			if err := uninstallSkillAt(target, home); err != nil {
				return err
			}
			if withHook {
				hookTarget := target
				if hookTarget == "agents" || hookTarget == "auto" {
					hookTarget = "all"
				}
				if err := uninstallHooksAt(hookTarget, home); err != nil {
					return err
				}
			}
			return nil
		},
	}
	c.Flags().StringVar(&target, "target", "all", "target tool(s): all|agents|claude|codex|antigravity")
	c.Flags().BoolVar(&withHook, "with-hook", true, "also remove the SessionStart hook")
	return c
}

func newSkillShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Print the embedded livegit SKILL.md to stdout",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := cmd.OutOrStdout().Write(docs.SkillMD)
			return err
		},
	}
}

func resolveSkillDirs(target, home string, includeMissing bool) ([]string, error) {
	type loc struct {
		name     string
		probeDir string
		skillDir string
	}
	locs := []loc{
		{"agents", filepath.Join(home, ".agents"), filepath.Join(home, ".agents", "skills", "livegit")},
		{"claude", filepath.Join(home, ".claude"), filepath.Join(home, ".claude", "skills", "livegit")},
		{"codex", filepath.Join(home, ".codex"), filepath.Join(home, ".codex", "skills", "livegit")},
		{"antigravity", filepath.Join(home, ".gemini", "antigravity-cli"), filepath.Join(home, ".gemini", "antigravity-cli", "skills", "livegit")},
	}

	switch target {
	case "agents", "claude", "codex", "antigravity":
		for _, l := range locs {
			if l.name == target {
				return []string{l.skillDir}, nil
			}
		}
		return nil, nil
	case "all":
		out := make([]string, 0, len(locs))
		for _, l := range locs {
			out = append(out, l.skillDir)
		}
		return out, nil
	case "auto":
		var out []string
		for _, l := range locs {
			if l.name == "agents" {
				out = append(out, l.skillDir)
				continue
			}
			if st, err := os.Stat(l.probeDir); err == nil && st.IsDir() {
				out = append(out, l.skillDir)
			}
		}
		if includeMissing && len(out) == 0 {
			out = append(out, locs[0].skillDir)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unknown --target %q (want auto|all|agents|claude|codex|antigravity)", target)
	}
}

func installSkillAt(target, home string) error {
	dirs, err := resolveSkillDirs(target, home, true)
	if err != nil {
		return err
	}
	files := docs.SkillFiles()
	for _, dir := range dirs {
		for _, f := range files {
			dst := filepath.Join(dir, f.Name)
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(dst, f.Content, 0o644); err != nil {
				return err
			}
		}
		fmt.Printf("✓ installed livegit skill in %s\n", dir)
	}
	return nil
}

func uninstallSkillAt(target, home string) error {
	dirs, err := resolveSkillDirs(target, home, false)
	if err != nil {
		return err
	}
	for _, dir := range dirs {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		fmt.Printf("✓ removed livegit skill from %s\n", dir)
	}
	return nil
}
