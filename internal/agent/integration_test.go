package agent

import (
	"bufio"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/taehyun/lg/internal/proto"
	"github.com/taehyun/lg/internal/transport"
)

// TestEndToEndOverPipe wires a real yamux Ghost<->Source over net.Pipe and
// exercises the framing, stream multiplexing, control ping, and file RPCs end
// to end — the in-memory equivalent of the S1 spike, run as a test.
func TestEndToEndOverPipe(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hi there"), 0o644)

	ghostConn, sourceConn := net.Pipe()

	srv, err := NewServer(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(sourceConn)

	sess, err := transport.NewClientSession(ghostConn)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	// Drain the server-opened notify stream so AcceptStream on the server keeps
	// flowing (the client must accept inbound streams).
	go func() {
		for {
			s, _, err := transport.AcceptStream(sess)
			if err != nil {
				return
			}
			go func() {
				br := bufio.NewReader(s)
				for {
					if _, err := proto.ReadFrame(br); err != nil {
						return
					}
				}
			}()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Control ping/pong.
	ctlStream, err := transport.OpenStream(sess, transport.StreamControl)
	if err != nil {
		t.Fatal(err)
	}
	ctl := transport.NewEndpoint(ctlStream)
	go ctl.Serve()
	pong, err := ctl.Call(ctx, proto.TypePing, proto.Ping{Nonce: 99})
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	var p proto.Pong
	proto.Unmarshal(pong.Body, &p)
	if p.Nonce != 99 {
		t.Fatalf("pong nonce=%d", p.Nonce)
	}

	// File RPC: stat + read existing, write new.
	fileStream, err := transport.OpenStream(sess, transport.StreamFile)
	if err != nil {
		t.Fatal(err)
	}
	file := transport.NewEndpoint(fileStream)
	go file.Serve()

	statF, err := file.Call(ctx, proto.TypeStatReq, proto.StatReq{Rel: "hello.txt"})
	if err != nil {
		t.Fatal(err)
	}
	var sr proto.StatResp
	proto.Unmarshal(statF.Body, &sr)
	if !sr.Stat.Exists || sr.Stat.Size != 8 {
		t.Fatalf("stat=%+v", sr.Stat)
	}

	readF, err := file.Call(ctx, proto.TypeReadReq, proto.ReadReq{Rel: "hello.txt"})
	if err != nil {
		t.Fatal(err)
	}
	var rr proto.ReadResp
	proto.Unmarshal(readF.Body, &rr)
	if string(rr.Content) != "hi there" {
		t.Fatalf("read=%q", rr.Content)
	}

	writeF, err := file.Call(ctx, proto.TypeWriteReq, proto.WriteReq{
		Rel: "sub/new.txt", Content: []byte("written"), ModTime: time.Now().Unix(), Mode: 0o644,
	})
	if err != nil {
		t.Fatal(err)
	}
	var wa proto.WriteAck
	proto.Unmarshal(writeF.Body, &wa)
	if !wa.OK {
		t.Fatalf("write ack=%+v", wa)
	}
	got, err := os.ReadFile(filepath.Join(root, "sub/new.txt"))
	if err != nil || string(got) != "written" {
		t.Fatalf("written file=%q err=%v", got, err)
	}

	// Full-tree metadata: the whole tree comes back in one request, including the
	// file we just wrote — the OneDrive-style eager listing.
	treeF, err := file.Call(ctx, proto.TypeTreeReq, proto.TreeReq{})
	if err != nil {
		t.Fatal(err)
	}
	var tr proto.TreeResp
	proto.Unmarshal(treeF.Body, &tr)
	seen := map[string]bool{}
	for _, e := range tr.Entries {
		seen[e.Rel] = true
	}
	if !seen["hello.txt"] || !seen["sub/new.txt"] || !seen["sub"] {
		t.Fatalf("tree missing entries: %+v", tr.Entries)
	}

	// Command runner: run a command in a remote PTY and read its output back.
	out, exitCode := runExecOverPipe(t, sess, ctx, "printf hello-exec", "")
	if exitCode != 0 || !strings.Contains(out, "hello-exec") {
		t.Fatalf("exec: code=%d out=%q", exitCode, out)
	}

	// Cwd: a command runs in the requested rel subdir (remote_root/sub), so that
	// `lg ls` under <mount>/sub lists Source's <repo>/sub. `sub` exists from the
	// earlier write of sub/new.txt.
	out, exitCode = runExecOverPipe(t, sess, ctx, "pwd", "sub")
	if exitCode != 0 || !strings.Contains(out, "/sub") {
		t.Fatalf("exec with cwd=sub: code=%d out=%q (want a path ending in /sub)", exitCode, out)
	}
}

// runExecOverPipe drives one ExecReq/ExecExit round-trip against the server's
// exec hub (in rel dir cwd) and returns the streamed output plus the exit code.
func runExecOverPipe(t *testing.T, sess *yamux.Session, ctx context.Context, cmd, cwd string) (string, int) {
	t.Helper()
	ctlStream, err := transport.OpenStream(sess, transport.StreamPTYCtl)
	if err != nil {
		t.Fatal(err)
	}
	dataStream, err := transport.OpenStream(sess, transport.StreamPTY)
	if err != nil {
		t.Fatal(err)
	}
	ctl := transport.NewEndpoint(ctlStream)
	exitCh := make(chan int, 1)
	ctl.SetHandler(func(f proto.Frame) (proto.MsgType, any, bool, error) {
		if f.Type == proto.TypeExecExit {
			var ex proto.ExecExit
			proto.Unmarshal(f.Body, &ex)
			select {
			case exitCh <- ex.Code:
			default:
			}
		}
		return 0, nil, false, nil
	})
	go ctl.Serve()

	resp, err := ctl.Call(ctx, proto.TypeExecReq, proto.ExecReq{Cmd: cmd, Cwd: cwd, Cols: 80, Rows: 24, Term: "xterm"})
	if err != nil {
		t.Fatal(err)
	}
	var er proto.ExecResp
	proto.Unmarshal(resp.Body, &er)
	if _, err := io.WriteString(dataStream, er.Token+"\n"); err != nil {
		t.Fatal(err)
	}

	// Drain output until the stream closes (process exit).
	out := make([]byte, 0, 64)
	buf := make([]byte, 256)
	for {
		n, rerr := dataStream.Read(buf)
		out = append(out, buf[:n]...)
		if rerr != nil {
			break
		}
		if len(out) > 4096 {
			break
		}
	}
	select {
	case code := <-exitCh:
		return string(out), code
	case <-time.After(2 * time.Second):
		t.Fatal("no ExecExit received")
		return "", -1
	}
}
