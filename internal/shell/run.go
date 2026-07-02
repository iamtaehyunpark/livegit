package shell

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/iamtaehyunpark/livegit/internal/logx"
	"github.com/iamtaehyunpark/livegit/internal/proto"
	"github.com/iamtaehyunpark/livegit/internal/transport"
	"golang.org/x/term"
)

// RunRemote runs one command on Source inside a real PTY and streams it to the
// local terminal, returning the remote process's exit code (the command runner).
//
// It mirrors `ssh host <cmd>`: the local terminal is put in raw mode so every
// keystroke — including Ctrl-C/Ctrl-Z — rides in-band to the remote PTY's line
// discipline (SIGINT reaches the remote process, not just the local client).
// Only SIGWINCH is out-of-band, sent as an explicit Resize so curses programs
// render at the right size. cmd is the shell command line; cwd is a rel path
// (mapped to the remote root) or "" for the root itself.
func RunRemote(client *transport.Client, cmd, cwd string) (int, error) {
	ctlStream, dataStream, err := client.OpenPTYStreams()
	if err != nil {
		return 1, err
	}
	defer ctlStream.Close()
	defer dataStream.Close()

	// exitCode is filled when the remote process exits (ExecExit push on ctl).
	var exitMu sync.Mutex
	exitCode := 0
	gotExit := false
	exited := make(chan struct{})
	ctl := transport.NewEndpoint(ctlStream)
	ctl.SetHandler(func(f proto.Frame) (proto.MsgType, any, bool, error) {
		if f.Type == proto.TypeExecExit {
			var ex proto.ExecExit
			_ = proto.Unmarshal(f.Body, &ex)
			exitMu.Lock()
			exitCode, gotExit = ex.Code, true
			exitMu.Unlock()
			select {
			case <-exited:
			default:
				close(exited)
			}
		}
		return 0, nil, false, nil
	})
	go ctl.Serve()

	cols, rows := terminalSize()
	termEnv := os.Getenv("TERM")
	if termEnv == "" {
		termEnv = "xterm-256color"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	resp, err := ctl.Call(ctx, proto.TypeExecReq, proto.ExecReq{
		Cmd: cmd, Cwd: cwd, Cols: cols, Rows: rows, Term: termEnv,
	})
	cancel()
	if err != nil {
		return 1, fmt.Errorf("start remote command: %w", err)
	}
	var er proto.ExecResp
	if err := proto.Unmarshal(resp.Body, &er); err != nil {
		return 1, err
	}

	// Pair the data stream to the invocation via the token line.
	if _, err := io.WriteString(dataStream, er.Token+"\n"); err != nil {
		return 1, err
	}

	// Raw mode so keystrokes pass through untouched (incl. Ctrl-C → remote PTY).
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		oldState, err := term.MakeRaw(fd)
		if err == nil {
			defer term.Restore(fd, oldState)
		}
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

	logx.For("run").Debug("remote command bridged", "cmd", cmd, "token", er.Token)

	// Pump bytes both ways; finish when the remote output side closes.
	go func() { _, _ = io.Copy(dataStream, os.Stdin) }()
	_, _ = io.Copy(os.Stdout, dataStream)

	// The data stream closed (process exited). Give the ExecExit push a brief
	// moment to arrive on the control stream so we propagate the real code.
	select {
	case <-exited:
	case <-time.After(500 * time.Millisecond):
	}
	exitMu.Lock()
	code := exitCode
	if !gotExit {
		code = 0 // clean stream close with no exit signal: treat as success
	}
	exitMu.Unlock()
	return code, nil
}

// terminalSize returns the current terminal dimensions (cols, rows).
func terminalSize() (uint16, uint16) {
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 || h <= 0 {
		return 80, 24
	}
	return uint16(w), uint16(h)
}
