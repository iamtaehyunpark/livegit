package cli

import (
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/taehyun/lg/internal/agent"
)

// newServeCmd is the Source-side daemon, invoked by Ghost over ssh as
// `lg serve --remote-root <path> [--ignore <csv>]` (see transport.dialSSH). It
// speaks the yamux protocol over stdin/stdout.
func newServeCmd() *cobra.Command {
	var remoteRoot, ignore string
	cmd := &cobra.Command{
		Use:    "serve",
		Short:  "Run the Source-side agent (spoken over ssh stdio)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			var patterns []string
			if ignore != "" {
				patterns = strings.Split(ignore, ",")
			}
			srv, err := agent.NewServer(remoteRoot, patterns)
			if err != nil {
				return err
			}
			conn := &stdioConn{in: os.Stdin, out: os.Stdout}
			return srv.Serve(conn)
		},
	}
	cmd.Flags().StringVar(&remoteRoot, "remote-root", "", "absolute repo path on Source (required)")
	cmd.Flags().StringVar(&ignore, "ignore", "", "comma-separated ignore patterns (from Ghost config)")
	_ = cmd.MarkFlagRequired("remote-root")
	return cmd
}

// stdioConn adapts os.Stdin/os.Stdout to an io.ReadWriteCloser for yamux.
type stdioConn struct {
	in  *os.File
	out *os.File
}

func (s *stdioConn) Read(b []byte) (int, error)  { return s.in.Read(b) }
func (s *stdioConn) Write(b []byte) (int, error) { return s.out.Write(b) }
func (s *stdioConn) Close() error {
	_ = s.in.Close()
	return s.out.Close()
}
