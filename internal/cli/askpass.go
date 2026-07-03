package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/iamtaehyunpark/livegit/internal/config"
	"github.com/iamtaehyunpark/livegit/internal/transport"
	"github.com/spf13/cobra"
)

// newAskpassCmd is the SSH_ASKPASS helper behind `lg connect`'s password
// auto-fill. ssh (with SSH_ASKPASS_REQUIRE=force) invokes it once per prompt,
// passing the prompt text as the argument and reading the answer from stdout:
//   - a password prompt is answered from the encrypted store (no typing);
//   - anything else — the Duo menu, a passcode, a host-key yes/no — is the
//     user's to answer, so it's relayed through the controlling terminal.
//
// Hidden: only ever launched by the shim script askpassEnv() writes, which
// pins the project via LG_HOME (ssh gives the helper an arbitrary cwd).
func newAskpassCmd() *cobra.Command {
	c := &cobra.Command{
		Use:                "askpass [prompt]",
		Short:              "SSH_ASKPASS helper (internal; used by `lg connect`)",
		Hidden:             true,
		Args:               cobra.ArbitraryArgs,
		DisableFlagParsing: true, // the prompt is arbitrary text; never parse it as flags
		RunE: func(cmd *cobra.Command, args []string) error {
			prompt := strings.Join(args, " ")

			if transport.PasswordLikeQuestion(prompt) {
				pw, err := config.LoadPassword()
				if err != nil {
					return err
				}
				if pw == "" {
					return fmt.Errorf("prompt %q looks like a password prompt but no password is stored (run `lg init --auth password`)", prompt)
				}
				fmt.Println(pw)
				return nil
			}

			// Second-auth / confirmation prompt: relay it to the real terminal.
			// stdout is reserved for the answer ssh reads, so the conversation
			// happens on /dev/tty directly.
			tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
			if err != nil {
				return fmt.Errorf("ssh asked %q but there is no terminal to answer on — run `lg connect` from a terminal (%v)", prompt, err)
			}
			defer tty.Close()
			fmt.Fprint(tty, prompt)
			if !strings.HasSuffix(prompt, " ") && !strings.HasSuffix(prompt, "\n") {
				fmt.Fprint(tty, " ")
			}
			line, err := bufio.NewReader(tty).ReadString('\n')
			if err != nil && line == "" {
				return err
			}
			fmt.Println(strings.TrimRight(line, "\r\n"))
			return nil
		},
	}
	return c
}
