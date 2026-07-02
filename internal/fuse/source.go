package fuse

import (
	"context"

	"github.com/iamtaehyunpark/livegit/internal/proto"
	"github.com/iamtaehyunpark/livegit/internal/transport"
)

// clientSource adapts a transport.Client to the SourceRPC interface the Backend
// consumes. Kept thin: marshal request, call, unmarshal response.
type clientSource struct {
	c *transport.Client
}

// NewClientSource wraps a transport client as a SourceRPC.
func NewClientSource(c *transport.Client) SourceRPC { return &clientSource{c: c} }

func (s *clientSource) Online() bool { return s.c.Status().Online() }

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

func (s *clientSource) Read(ctx context.Context, rel string) (proto.ReadResp, error) {
	f, err := s.c.FileCall(ctx, proto.TypeReadReq, proto.ReadReq{Rel: rel})
	if err != nil {
		return proto.ReadResp{}, err
	}
	var resp proto.ReadResp
	err = proto.Unmarshal(f.Body, &resp)
	return resp, err
}

func (s *clientSource) Write(ctx context.Context, req proto.WriteReq) (proto.WriteAck, error) {
	f, err := s.c.FileCall(ctx, proto.TypeWriteReq, req)
	if err != nil {
		return proto.WriteAck{}, err
	}
	var ack proto.WriteAck
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

func (s *clientSource) Tree(ctx context.Context) ([]proto.TreeEntry, error) {
	f, err := s.c.FileCall(ctx, proto.TypeTreeReq, proto.TreeReq{})
	if err != nil {
		return nil, err
	}
	var resp proto.TreeResp
	if err := proto.Unmarshal(f.Body, &resp); err != nil {
		return nil, err
	}
	return resp.Entries, nil
}
