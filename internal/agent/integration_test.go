package agent

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

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

	srv, err := NewServer(root)
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
}
