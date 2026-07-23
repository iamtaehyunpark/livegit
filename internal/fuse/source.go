package fuse

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"

	"github.com/iamtaehyunpark/livegit/internal/proto"
	"github.com/iamtaehyunpark/livegit/internal/transport"
)

// clientSource adapts a transport.Client to the SourceRPC interface the Backend
// consumes. The chunk/page loops for big transfers live HERE, behind the plain
// whole-value SourceRPC signatures, so the Backend never sees the wire shape.
type clientSource struct {
	c *transport.Client
}

// NewClientSource wraps a transport client as a SourceRPC.
func NewClientSource(c *transport.Client) SourceRPC { return &clientSource{c: c} }

func (s *clientSource) Online() bool { return s.c.Status().Online() }

// fileCaller is the one-frame request/response primitive the transfer loops
// run on (transport.Client.FileCall in production, a fake in tests).
type fileCaller func(ctx context.Context, t proto.MsgType, body any) (proto.Frame, error)

func (s *clientSource) Stat(ctx context.Context, rel string) (proto.FileStat, error) {
	f, err := s.c.FileCall(ctx, proto.TypeStatReq, proto.StatReq{Rel: rel})
	if err != nil {
		return proto.FileStat{}, err
	}
	var resp proto.StatResp
	if err := proto.Unmarshal(f.Body, &resp); err != nil {
		return proto.FileStat{}, err
	}
	return resp.Stat, nil
}

func (s *clientSource) ReadStream(ctx context.Context, rel string, sink func([]byte) error) (proto.FileStat, error) {
	return readStream(ctx, s.c.FileCall, rel, 0, sink)
}

func (s *clientSource) Write(ctx context.Context, req proto.WriteReq) (proto.WriteAck, error) {
	return writeChunked(ctx, s.c.FileCall, req, proto.ChunkSize)
}

func (s *clientSource) Rename(ctx context.Context, req proto.RenameReq) (proto.RenameAck, error) {
	f, err := s.c.FileCall(ctx, proto.TypeRenameReq, req)
	if err != nil {
		return proto.RenameAck{}, err
	}
	var ack proto.RenameAck
	err = proto.Unmarshal(f.Body, &ack)
	return ack, err
}

func (s *clientSource) Delete(ctx context.Context, req proto.DelReq) (proto.DelAck, error) {
	f, err := s.c.FileCall(ctx, proto.TypeDelReq, req)
	if err != nil {
		return proto.DelAck{}, err
	}
	var ack proto.DelAck
	err = proto.Unmarshal(f.Body, &ack)
	return ack, err
}

func (s *clientSource) Tree(ctx context.Context, have string) ([]proto.TreeEntry, string, bool, error) {
	return fetchTree(ctx, s.c.FileCall, have)
}

// readStream fetches a file as a sequence of bounded chunks (maxLen 0 = agent
// default), delivering each to sink in order and returning the file metadata.
// One giant frame for a 200MB+ file used to blow the codec's 256MiB cap after
// base64 and kill the whole connection; chunking also bounds memory on both
// sides. The next chunk is requested BEFORE sink processes the current one
// (depth-1 prefetch), overlapping network transfer with disk writes. A file
// that changes on Source mid-fetch fails the read — readers may already have
// consumed early bytes, so a silent restart could hand out a torn mix.
func readStream(ctx context.Context, call fileCaller, rel string, maxLen int64, sink func([]byte) error) (proto.FileStat, error) {
	type result struct {
		chunk proto.ReadResp
		err   error
	}
	fetch := func(off int64, out chan<- result) {
		f, err := call(ctx, proto.TypeReadReq, proto.ReadReq{Rel: rel, Offset: off, MaxLen: maxLen})
		if err != nil {
			out <- result{err: err}
			return
		}
		var chunk proto.ReadResp
		err = proto.Unmarshal(f.Body, &chunk)
		out <- result{chunk: chunk, err: err}
	}

	var st proto.FileStat
	var off int64
	next := make(chan result, 1)
	go fetch(0, next)
	for first := true; ; first = false {
		r := <-next
		if r.err != nil {
			return proto.FileStat{}, r.err
		}
		chunk := r.chunk
		if !chunk.Found {
			if first {
				return proto.FileStat{Rel: rel}, nil // Exists=false
			}
			return proto.FileStat{}, fmt.Errorf("%s vanished on source during fetch", rel)
		}
		if first {
			st = proto.FileStat{Rel: rel, Exists: true, Size: chunk.Size,
				ModTime: chunk.ModTime, Mode: chunk.Mode}
		} else if chunk.Size != st.Size || chunk.ModTime != st.ModTime {
			return proto.FileStat{}, fmt.Errorf("%s changed on source during fetch", rel)
		}
		if chunk.More {
			if len(chunk.Content) == 0 {
				return proto.FileStat{}, fmt.Errorf("empty non-final read chunk for %s at offset %d", rel, off)
			}
			go fetch(off+int64(len(chunk.Content)), next) // prefetch while sink runs
		}
		if err := sink(chunk.Content); err != nil {
			if chunk.More {
				<-next // drain the prefetch so its goroutine can exit
			}
			return proto.FileStat{}, err
		}
		off += int64(len(chunk.Content))
		if !chunk.More {
			return st, nil
		}
	}
}

// writeChunked uploads req.Content in bounded pieces; the final (!More) chunk
// makes the agent commit atomically (conflict check + rename into place).
// Small writes and mkdirs stay a single frame, exactly as before.
func writeChunked(ctx context.Context, call fileCaller, req proto.WriteReq, chunkSize int) (proto.WriteAck, error) {
	writeCall := func(r proto.WriteReq) (proto.WriteAck, error) {
		f, err := call(ctx, proto.TypeWriteReq, r)
		if err != nil {
			return proto.WriteAck{}, err
		}
		var ack proto.WriteAck
		err = proto.Unmarshal(f.Body, &ack)
		return ack, err
	}
	if req.IsDir || len(req.Content) <= chunkSize {
		return writeCall(req) // zero Offset/More already mean "complete write"
	}
	content := req.Content
	for off := 0; ; {
		end := min(off+chunkSize, len(content))
		chunk := req // scalar fields (BaseHash/ModTime/Mode) ride on every chunk
		chunk.Content = content[off:end]
		chunk.Offset = int64(off)
		chunk.More = end < len(content)
		ack, err := writeCall(chunk)
		if err != nil || !chunk.More {
			return ack, err
		}
		off = end
	}
}

// fetchTree pulls the full-tree snapshot as gzipped pages, short-circuiting to
// unchanged=true when the agent's fresh walk matches the digest we already
// hold (then nothing but the two digest strings crosses the wire).
func fetchTree(ctx context.Context, call fileCaller, have string) ([]proto.TreeEntry, string, bool, error) {
	treeCall := func(req proto.TreeReq) (proto.TreeResp, error) {
		f, err := call(ctx, proto.TypeTreeReq, req)
		if err != nil {
			return proto.TreeResp{}, err
		}
		var resp proto.TreeResp
		err = proto.Unmarshal(f.Body, &resp)
		return resp, err
	}
	first, err := treeCall(proto.TreeReq{Digest: have})
	if err != nil {
		return nil, "", false, err
	}
	if first.Unchanged {
		return nil, have, true, nil
	}
	entries, err := decodeTreePage(first.Gz)
	if err != nil {
		return nil, "", false, err
	}
	for cur := 1; cur < first.Pages; cur++ {
		page, err := treeCall(proto.TreeReq{Digest: first.Digest, Cursor: cur})
		if err != nil {
			// Includes "snapshot expired" (a newer walk replaced ours):
			// surface it; RunTreeSync retries from cursor 0 on the next tick.
			return nil, "", false, err
		}
		more, err := decodeTreePage(page.Gz)
		if err != nil {
			return nil, "", false, err
		}
		entries = append(entries, more...)
	}
	return entries, first.Digest, false, nil
}

func decodeTreePage(gz []byte) ([]proto.TreeEntry, error) {
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var out []proto.TreeEntry
	if err := json.NewDecoder(zr).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}
