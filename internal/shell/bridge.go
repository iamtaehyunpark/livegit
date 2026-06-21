package shell

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/taehyun/lg/internal/logx"
	"github.com/taehyun/lg/internal/proto"
	"github.com/taehyun/lg/internal/transport"
	"golang.org/x/term"
)

// Bridge connects the local terminal to a remote tmux session over the yamux
// PTY streams (§5.3). It returns the session name so the caller can persist it.
//
// Signals ride in-band: the local terminal is put in raw mode and bytes are
// forwarded verbatim, so Ctrl-C/Ctrl-Z reach the remote pty's line discipline
// directly (§5.3, §10 "SSH already handles PTY/signals"). Only SIGWINCH is
// out-of-band, sent as an explicit Resize.
func Bridge(client *transport.Client, project, tabID, initialInput string) (string, error) {
	ctlStream, dataStream, err := client.OpenPTYStreams()
	if err != nil {
		return "", err
	}
	defer ctlStream.Close()
	defer dataStream.Close()

	ctl := transport.NewEndpoint(ctlStream)
	go ctl.Serve()

	cols, rows := terminalSize()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	resp, err := ctl.Call(ctx, proto.TypeSessionReq, proto.SessionReq{
		Project: project, TabID: tabID, Cols: cols, Rows: rows,
	})
	cancel()
	if err != nil {
		return "", fmt.Errorf("request session: %w", err)
	}
	var sr proto.SessionResp
	if err := proto.Unmarshal(resp.Body, &sr); err != nil {
		return "", err
	}

	// Pair the data stream to the session via the token line.
	if _, err := io.WriteString(dataStream, sr.Name+"\n"); err != nil {
		return sr.Name, err
	}
	// Forward the triggering command so it actually runs on Source (e.g. the
	// `conda activate` that caused entry) — §5.3/§5.5.
	if initialInput != "" {
		_, _ = io.WriteString(dataStream, initialInput+"\n")
	}

	// Raw mode on the local terminal so keystrokes pass through untouched.
	fd := int(os.Stdin.Fd())
	var oldState *term.State
	if term.IsTerminal(fd) {
		oldState, err = term.MakeRaw(fd)
		if err != nil {
			return sr.Name, err
		}
		defer term.Restore(fd, oldState)
	}

	// Forward SIGWINCH as Resize messages.
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			c, r := terminalSize()
			_ = ctl.Notify(proto.TypeResize, proto.Resize{Cols: c, Rows: r})
		}
	}()

	logx.For("shell").Debug("source session bridged", "session", sr.Name, "created", sr.Created)

	// Pump bytes both ways; finish when the remote side closes (detach/exit).
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(dataStream, os.Stdin); done <- struct{}{} }()
	go func() { _, _ = io.Copy(os.Stdout, dataStream); done <- struct{}{} }()
	<-done
	return sr.Name, nil
}

// terminalSize returns the current terminal dimensions (cols, rows).
func terminalSize() (uint16, uint16) {
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 || h <= 0 {
		return 80, 24
	}
	return uint16(w), uint16(h)
}
