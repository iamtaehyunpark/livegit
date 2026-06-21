package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/taehyun/lg/internal/config"
	"github.com/taehyun/lg/internal/shell"
)

// newHookCmd groups the fast, short-lived callbacks the shell integration runs
// per command (§5.1). They must be cheap: load config, check, print a directive.
func newHookCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "hook", Short: "shell-integration callbacks (internal)", Hidden: true}
	cmd.AddCommand(newHookCheckCmd(), newHookIsSourceCmd())
	return cmd
}

func newHookCheckCmd() *cobra.Command {
	var tab, cwd string
	c := &cobra.Command{
		Use:   "check -- <command>",
		Short: "evaluate a command for SOURCE-mode entry; prints 'ENTER <via>' or nothing",
		RunE: func(cmd *cobra.Command, args []string) error {
			command := strings.Join(args, " ")
			cfg, err := config.Load()
			if err != nil || cfg.Role != config.RoleGhost {
				return nil // silent: integration is a no-op without ghost config
			}
			st := shell.LoadState(tab)
			if st.Mode == shell.ModeSource {
				return nil // already in SOURCE mode; nothing to do
			}
			engine := shell.NewTriggerEngine(cfg)
			router := shell.NewRouter(cfg, engine)
			mapper := config.NewPathMapper(cfg)
			relDir := relDirOf(mapper, cwd)

			// Directory-marker auto-triggering is disabled (presence=nil). The
			// rule "marker absent on Ghost but present on Source" can't be
			// evaluated correctly from the hook: markers like .venv are ignored
			// (never materialized locally), so "absent on Ghost" is ALWAYS true,
			// which made nearly every non-readonly command flip to SOURCE — even
			// outside the mount. Doing it right needs a Source-side presence
			// check; until then we rely on the explicit, reliable triggers
			// (conda/venv/poetry/pyenv activation, always_source patterns).
			readonly := router.Classify(command) == shell.ClassReadonly
			d := engine.Evaluate(command, relDir, readonly, nil)
			if d.Enter {
				fmt.Fprintf(os.Stdout, "ENTER %s\n", d.Via)
			}
			return nil
		},
	}
	c.Flags().StringVar(&tab, "tab", "", "terminal tab id")
	c.Flags().StringVar(&cwd, "cwd", "", "current working directory")
	return c
}

func newHookIsSourceCmd() *cobra.Command {
	var tab string
	c := &cobra.Command{
		Use:   "is-source",
		Short: "exit 0 if the tab is in SOURCE mode",
		RunE: func(cmd *cobra.Command, args []string) error {
			st := shell.LoadState(tab)
			if st.Mode == shell.ModeSource {
				return nil
			}
			os.Exit(1)
			return nil
		},
	}
	c.Flags().StringVar(&tab, "tab", "", "terminal tab id")
	return c
}

// relDirOf converts an absolute cwd to a rel dir under local_root, or "" if the
// cwd is outside the mount (in which case dir-marker triggers don't apply).
func relDirOf(mapper *config.PathMapper, cwd string) string {
	if cwd == "" {
		return ""
	}
	rel, err := mapper.LocalToRel(cwd)
	if err != nil {
		return ""
	}
	return rel
}
