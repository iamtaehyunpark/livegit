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

// A file bigger than the chunk size streams through readStream in multiple
// ordered chunks, byte-identical, with correct metadata.
func TestReadStreamChunksRoundTrip(t *testing.T) {
	root := t.TempDir()
	content := bytes.Repeat([]byte("0123456789abcdef"), 64) // 1 KiB
	if err := os.WriteFile(filepath.Join(root, "big.bin"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	call := fsCaller(t, agent.NewFileServer(root, nil))

	var got []byte
	chunks := 0
	st, err := readStream(context.Background(), call, "big.bin", 100, func(c []byte) error {
		chunks++
		got = append(got, c...)
		return nil
	})
	if err != nil || !st.Exists {
		t.Fatalf("st=%+v err=%v", st, err)
	}
	if !bytes.Equal(got, content) || chunks < 2 {
		t.Fatalf("got %d bytes in %d chunks, want %d bytes in >1 chunks", len(got), chunks, len(content))
	}
	if st.Size != int64(len(content)) {
		t.Fatalf("size=%d want %d", st.Size, len(content))
	}

	missing, err := readStream(context.Background(), call, "nope.bin", 100, func([]byte) error {
		t.Fatal("sink must not run for a missing file")
		return nil
	})
	if err != nil || missing.Exists {
		t.Fatalf("missing file: st=%+v err=%v", missing, err)
	}
}

// writeFile streams a local file in chunks and the agent commits them as one
// atomic file; an upload cut mid-way RESUMES from the staged prefix on retry
// instead of re-sending from byte zero.
func TestWriteFileResumesAfterInterrupt(t *testing.T) {
	root := t.TempDir()
	realCall := fsCaller(t, agent.NewFileServer(root, nil))
	content := bytes.Repeat([]byte("wxyz"), 300) // 1200 bytes, chunkSize 500 -> 3 chunks
	local := filepath.Join(t.TempDir(), "up.bin")
	if err := os.WriteFile(local, content, 0o644); err != nil {
		t.Fatal(err)
	}

	// Flaky transport: drop the SECOND content chunk of the first attempt.
	var contentChunks, zeroStarts int
	failAt := 2
	flaky := func(ctx context.Context, typ proto.MsgType, body any) (proto.Frame, error) {
		if w, ok := body.(proto.WriteReq); ok && typ == proto.TypeWriteReq && !w.Probe {
			contentChunks++
			if w.Offset == 0 {
				zeroStarts++
			}
			if contentChunks == failAt {
				contentChunks = -1000 // fail only once
				return proto.Frame{}, context.DeadlineExceeded
			}
		}
		return realCall(ctx, typ, body)
	}

	req := proto.WriteReq{Rel: "up.bin", Mode: 0o644, ModTime: 1700000000}
	if _, err := writeFile(context.Background(), flaky, req, local, 500); err == nil {
		t.Fatal("first attempt should fail at chunk 2")
	}
	// Retry (what the flush worker does): must probe, resume at 500, and NOT
	// send another offset-0 chunk.
	ack, err := writeFile(context.Background(), flaky, req, local, 500)
	if err != nil || !ack.OK {
		t.Fatalf("retry ack=%+v err=%v", ack, err)
	}
	if zeroStarts != 1 {
		t.Fatalf("offset-0 chunks sent=%d, want 1 (retry must resume, not restart)", zeroStarts)
	}
	got, err := os.ReadFile(filepath.Join(root, "up.bin"))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("committed %d bytes err=%v", len(got), err)
	}
	if ack.NewHash != hashx.Bytes(content) {
		t.Fatalf("NewHash=%q want %q", ack.NewHash, hashx.Bytes(content))
	}
	if _, err := os.Stat(filepath.Join(root, "up.bin.lg-part.id")); !os.IsNotExist(err) {
		t.Fatal("stage id file must be gone after commit")
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
