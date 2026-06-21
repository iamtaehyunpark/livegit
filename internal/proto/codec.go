package proto

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// Wire format (D3 — hand-rolled length-prefixed framing):
//
//	uvarint  payloadLen
//	byte     type
//	8 bytes  reqID (big-endian)
//	N bytes  body
//
// The body is JSON for debuggability in this single-user tool; because the
// framing is independent of the body encoding, a protobuf-marshalled body can
// replace JSON later without changing readers/writers or the dispatch loop.

const maxFrameSize = 256 << 20 // 256 MiB guard; whole-file fetches can be large

// WriteFrame encodes and writes one frame. Safe for concurrent use only if the
// underlying writer is; callers serialize writes per stream.
func WriteFrame(w io.Writer, f Frame) error {
	header := make([]byte, 9)
	header[0] = byte(f.Type)
	binary.BigEndian.PutUint64(header[1:], f.ReqID)

	payloadLen := len(header) + len(f.Body)
	var lenBuf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(lenBuf[:], uint64(payloadLen))

	if _, err := w.Write(lenBuf[:n]); err != nil {
		return err
	}
	if _, err := w.Write(header); err != nil {
		return err
	}
	if len(f.Body) > 0 {
		if _, err := w.Write(f.Body); err != nil {
			return err
		}
	}
	return nil
}

// ReadFrame reads one frame. The reader should be buffered (ReadByte is used to
// decode the uvarint length).
func ReadFrame(r *bufio.Reader) (Frame, error) {
	payloadLen, err := binary.ReadUvarint(r)
	if err != nil {
		return Frame{}, err
	}
	if payloadLen < 9 || payloadLen > maxFrameSize {
		return Frame{}, fmt.Errorf("invalid frame length %d", payloadLen)
	}
	buf := make([]byte, payloadLen)
	if _, err := io.ReadFull(r, buf); err != nil {
		return Frame{}, err
	}
	f := Frame{
		Type:  MsgType(buf[0]),
		ReqID: binary.BigEndian.Uint64(buf[1:9]),
		Body:  buf[9:],
	}
	return f, nil
}

// Marshal encodes a message body.
func Marshal(v any) ([]byte, error) { return json.Marshal(v) }

// Unmarshal decodes a frame body into v.
func Unmarshal(body []byte, v any) error {
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, v)
}

// NewFrame builds a frame with an encoded body.
func NewFrame(t MsgType, reqID uint64, body any) (Frame, error) {
	b, err := Marshal(body)
	if err != nil {
		return Frame{}, err
	}
	return Frame{Type: t, ReqID: reqID, Body: b}, nil
}
