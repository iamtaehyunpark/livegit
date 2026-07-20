package cli

import (
	"fmt"
	"time"

	"github.com/iamtaehyunpark/livegit/internal/shell"
	"github.com/spf13/cobra"
)

// newJobsCmd is `lg jobs` — the async counterpart to `lg run`. It lists jobs
// launched with `lg run --detach`, which run on Source under systemd --user so
// they outlive the launching command (and the ghost disconnecting). `kill` and
// `rm` manage their lifecycle.
func newJobsCmd() *cobra.Command {
	var limit int
	var all bool
	cmd := &cobra.Command{
		Use:   "jobs",
		Short: "List detached remote jobs (started with `lg run --detach`)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := connectedClient(15 * time.Second)
			if err != nil {
				return err
			}
			defer client.Close()
			jobs, err := shell.ListJobs(client)
			if err != nil {
				return err
			}
			n := limit
			if all {
				n = 0
			}
			fmt.Print(shell.FormatJobs(jobs, n))
			return nil
		},
	}
	cmd.Flags().IntVarP(&limit, "limit", "n", 20, "max jobs to show (most recent kept)")
	cmd.Flags().BoolVar(&all, "all", false, "show the full job list, ignoring --limit")
	cmd.AddCommand(
		newJobsActCmd("kill <id>", "Stop a running job", "kill"),
		newJobsActCmd("rm <id>", "Remove a finished job and delete its logs", "rm"),
	)
	return cmd
}

func newJobsActCmd(use, short, action string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := connectedClient(15 * time.Second)
			if err != nil {
				return err
			}
			defer client.Close()
			msg, err := shell.ActJob(client, args[0], action)
			if err != nil {
				return err
			}
			fmt.Printf("%s: %s\n", args[0], msg)
			return nil
		},
	}
}

// newLogsCmd is `lg logs <id>` — show (or, with -f, follow) a detached job's
// output. Following ends when the job finishes; Ctrl-C stops following without
// touching the job.
func newLogsCmd() *cobra.Command {
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs <id>",
		Short: "Show a detached job's output (-f to follow live)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := connectedClient(15 * time.Second)
			if err != nil {
				return err
			}
			defer client.Close()
			return shell.StreamLog(client, args[0], follow)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "stream new output until the job finishes")
	return cmd
}
