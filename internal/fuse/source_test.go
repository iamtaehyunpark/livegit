package fuse

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/iamtaehyunpark/livegit/internal/agent"
	"github.com/iamtaehyunpark/livegit/internal/hashx"
	"github.com/iamtaehyunpark/livegit/internal/proto"
)

// fsCaller adapts a real agent FileServer as a fileCaller, so the ghost-side
// chunk/page loops are tested against the actual agent-side handlers (both
// halves of the wire logic, no transport in between).
func fsCaller(t *testing.T, fs *agent.FileServer) fileCaller {
	return func(_ context.Context, typ proto.MsgType, body any) (proto.Frame, error) {
		t.Helper()
		b, err := proto.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		respType, resp, _, err := fs.Handle(proto.Frame{Type: typ, Body: b})
		if err != nil {
			return proto.Frame{}, err
		}
		rb, err := proto.Marshal(resp)
		if err != nil {
			t.Fatal(err)
		}
		return proto.Frame{Type: respType, Body: rb}, nil
	}
}

// A file bigger than the chunk size round-trips through readFull in multiple
// chunks, byte-identical, with the whole-file hash computed ghost-side.
func TestReadFullChunksRoundTrip(t *testing.T) {
	root := t.TempDir()
	content := bytes.Repeat([]byte("0123456789abcdef"), 64) // 1 KiB
	if err := os.WriteFile(filepath.Join(root, "big.bin"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	call := fsCaller(t, agent.NewFileServer(root, nil))

	resp, err := readFull(context.Background(), call, "big.bin", 100) // force ~11 chunks
	if err != nil || !resp.Found {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if !bytes.Equal(resp.Content, content) {
		t.Fatalf("content mismatch: got %d bytes want %d", len(resp.Content), len(content))
	}
	if resp.Hash != hashx.Bytes(content) {
		t.Fatalf("hash=%q want %q", resp.Hash, hashx.Bytes(content))
	}

	missing, err := readFull(context.Background(), call, "nope.bin", 100)
	if err != nil || missing.Found {
		t.Fatalf("missing file: resp=%+v err=%v", missing, err)
	}
}

// writeChunked splits content into chunks and the agent commits them as one
// atomic file.
func TestWriteChunkedRoundTrip(t *testing.T) {
	root := t.TempDir()
	call := fsCaller(t, agent.NewFileServer(root, nil))
	content := bytes.Repeat([]byte("wxyz"), 300) // 1200 bytes, chunkSize 500 -> 3 chunks

	ack, err := writeChunked(context.Background(), call,
		proto.WriteReq{Rel: "up.bin", Content: content, Mode: 0o644, ModTime: 1700000000}, 500)
	if err != nil || !ack.OK {
		t.Fatalf("ack=%+v err=%v", ack, err)
	}
	got, err := os.ReadFile(filepath.Join(root, "up.bin"))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("committed %d bytes err=%v", len(got), err)
	}
	if ack.NewHash != hashx.Bytes(content) {
		t.Fatalf("NewHash=%q want %q", ack.NewHash, hashx.Bytes(content))
	}
}

// fetchTree assembles the paged snapshot and short-circuits to unchanged on a
// digest match.
func TestFetchTreeAndDigestSkip(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"a.txt", "sub/b.txt"} {
		abs := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(p), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	call := fsCaller(t, agent.NewFileServer(root, nil))

	entries, digest, unchanged, err := fetchTree(context.Background(), call, "")
	if err != nil || unchanged || digest == "" {
		t.Fatalf("digest=%q unchanged=%v err=%v", digest, unchanged, err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Rel] = true
	}
	if !seen["a.txt"] || !seen["sub"] || !seen["sub/b.txt"] {
		t.Fatalf("entries=%+v", entries)
	}

	entries, digest2, unchanged, err := fetchTree(context.Background(), call, digest)
	if err != nil || !unchanged || digest2 != digest || entries != nil {
		t.Fatalf("skip: entries=%v digest2=%q unchanged=%v err=%v", entries, digest2, unchanged, err)
	}
}
