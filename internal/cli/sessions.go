package cli

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/taehyun/lg/internal/proto"
)

// newSessionsCmd lists the tmux sessions lg has created on Source (§5.7).
func newSessionsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sessions",
		Short: "List remote tmux sessions created by lg",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := loadGhost()
			if err != nil {
				return err
			}
			client := newClient(c)
			defer client.Close()
			if err := waitOnline(client, 10*time.Second); err != nil {
				return fmt.Errorf("offline: %w", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			// Session listing rides the file-RPC path's control type; reuse the
			// control endpoint via a session-list request.
			f, err := client.ControlCall(ctx, proto.TypeSessionList, proto.SessionListReq{})
			if err != nil {
				return err
			}
			var resp proto.SessionsResp
			if err := proto.Unmarshal(f.Body, &resp); err != nil {
				return err
			}
			if len(resp.Sessions) == 0 {
				fmt.Println("no sessions")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "SESSION\tATTACHED\tWINDOWS\tCREATED")
			for _, s := range resp.Sessions {
				created := time.Unix(s.Created, 0).Format("01-02 15:04")
				fmt.Fprintf(w, "%s\t%v\t%d\t%s\n", s.Name, s.Attached, s.Windows, created)
			}
			return w.Flush()
		},
	}
}
