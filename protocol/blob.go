package protocol

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
)

const (
	// MaxChunk is the largest payload in a single binary frame. Chunking exists
	// so a large transfer cannot starve a permission prompt on the same
	// connection; a bigger chunk defeats that.
	MaxChunk = 256 << 10 // 256 KiB

	// MaxBlobSize caps a whole transfer. Above this, reject with a clear
	// message rather than chunking indefinitely.
	MaxBlobSize = 50 << 20 // 50 MiB

	// MaxBlobComment caps the message sent alongside an outbound file. Slack's
	// initial_comment truncates well below this; the cap is here so a runaway
	// agent cannot push an unbounded string through a control frame.
	MaxBlobComment = 3000

	// maxChunkHeader bounds the JSON header so a malformed length prefix cannot
	// make the reader allocate arbitrarily.
	maxChunkHeader = 4 << 10
)

// ChunkHeader identifies which transfer a binary frame belongs to. Field names
// are short because this header is repeated once per chunk.
type ChunkHeader struct {
	BlobID string `json:"b"`
	Seq    uint32 `json:"n"`
}

// EncodeChunk builds a binary frame:
//
//	[4 bytes big-endian header length][JSON header][raw payload]
//
// Streams are multiplexed by BlobID, so chunks from different transfers may
// interleave on one connection.
func EncodeChunk(h ChunkHeader, payload []byte) ([]byte, error) {
	if h.BlobID == "" {
		return nil, errors.New("protocol: chunk header requires a blob id")
	}
	if len(payload) == 0 {
		return nil, errors.New("protocol: refusing to encode an empty chunk")
	}
	if len(payload) > MaxChunk {
		return nil, fmt.Errorf("protocol: chunk of %d bytes exceeds cap %d", len(payload), MaxChunk)
	}
	hdr, err := json.Marshal(h)
	if err != nil {
		return nil, err
	}
	if len(hdr) > maxChunkHeader {
		return nil, fmt.Errorf("protocol: chunk header of %d bytes exceeds cap %d", len(hdr), maxChunkHeader)
	}
	out := make([]byte, 4+len(hdr)+len(payload))
	binary.BigEndian.PutUint32(out[:4], uint32(len(hdr)))
	copy(out[4:], hdr)
	copy(out[4+len(hdr):], payload)
	return out, nil
}

// DecodeChunk parses a binary frame. The returned payload aliases the input
// buffer; copy it if the caller retains it beyond the read loop.
func DecodeChunk(frame []byte) (ChunkHeader, []byte, error) {
	var h ChunkHeader
	if len(frame) < 4 {
		return h, nil, errors.New("protocol: truncated chunk frame")
	}
	n := binary.BigEndian.Uint32(frame[:4])
	if n == 0 {
		return h, nil, errors.New("protocol: chunk frame has a zero-length header")
	}
	if n > maxChunkHeader {
		return h, nil, fmt.Errorf("protocol: chunk header length %d exceeds cap %d", n, maxChunkHeader)
	}
	if uint64(len(frame)) < uint64(4)+uint64(n) {
		return h, nil, errors.New("protocol: chunk frame truncated inside header")
	}
	if err := json.Unmarshal(frame[4:4+n], &h); err != nil {
		return h, nil, fmt.Errorf("protocol: malformed chunk header: %w", err)
	}
	if h.BlobID == "" {
		return h, nil, errors.New("protocol: chunk header is missing a blob id")
	}
	payload := frame[4+n:]
	if len(payload) == 0 {
		return h, nil, errors.New("protocol: chunk frame carries no payload")
	}
	if len(payload) > MaxChunk {
		return h, nil, fmt.Errorf("protocol: chunk payload of %d bytes exceeds cap %d", len(payload), MaxChunk)
	}
	return h, payload, nil
}
