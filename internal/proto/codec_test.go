package proto

import (
	"bufio"
	"bytes"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := []Frame{
		{Type: TypePing, ReqID: 1, Body: []byte(`{"Nonce":7}`)},
		{Type: TypeReadResp, ReqID: 42, Body: bytes.Repeat([]byte("x"), 100000)},
		{Type: TypeInvalidate, ReqID: 0, Body: nil},
	}
	for _, f := range in {
		if err := WriteFrame(&buf, f); err != nil {
			t.Fatal(err)
		}
	}
	br := bufio.NewReader(&buf)
	for i, want := range in {
		got, err := ReadFrame(br)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if got.Type != want.Type || got.ReqID != want.ReqID || !bytes.Equal(got.Body, want.Body) {
			t.Errorf("frame %d mismatch: got {%d,%d,%dB} want {%d,%d,%dB}",
				i, got.Type, got.ReqID, len(got.Body), want.Type, want.ReqID, len(want.Body))
		}
	}
}

func TestNewFrameMarshal(t *testing.T) {
	f, err := NewFrame(TypeStatReq, 5, StatReq{Rel: "a/b.go"})
	if err != nil {
		t.Fatal(err)
	}
	var req StatReq
	if err := Unmarshal(f.Body, &req); err != nil {
		t.Fatal(err)
	}
	if req.Rel != "a/b.go" {
		t.Errorf("rel=%q", req.Rel)
	}
}
