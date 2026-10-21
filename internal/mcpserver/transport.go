package mcpserver

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var (
	errRequestTooLarge = errors.New("MCP request exceeds max_request_bytes")
	errMalformedFrame  = errors.New("malformed MCP request frame")
)

// boundedFrameReader is an io.ReadCloser for newline-delimited JSON. It reads
// at most max payload bytes per frame, where the payload excludes LF or CRLF
// framing. Each line is additionally required to be one complete JSON value
// before it is exposed to the SDK.
//
// The JSON check is a framing guard, not protocol decoding. Without it, the
// SDK's json.Decoder could join multiple physical lines into one JSON value
// and evade the per-frame byte limit. json.Valid scans the already-bounded
// line without unmarshalling or allocating a second copy of the payload.
type boundedFrameReader struct {
	r   io.ReadCloser
	br  *bufio.Reader
	max int

	// pending holds the current frame plus its normalized LF terminator.
	// Only one frame is exposed at a time.
	pending []byte
	off     int

	done        bool
	terminalErr error

	closeOnce sync.Once
	closeErr  error
}

func newBoundedFrameReader(r io.ReadCloser, max int) *boundedFrameReader {
	return &boundedFrameReader{
		r:   r,
		br:  bufio.NewReader(r),
		max: max,
	}
}

// newBoundedTransport wraps reader and writer in the SDK's IOTransport. The
// SDK still owns JSON-RPC decoding, request/response correlation, lifecycle
// handling and legacy batch semantics; only the inbound byte boundary is
// custom.
func newBoundedTransport(reader io.ReadCloser, writer io.Writer, max int) mcp.Transport {
	return &mcp.IOTransport{
		Reader: newBoundedFrameReader(reader, max),
		Writer: nopWriteCloser{Writer: writer},
	}
}

type nopWriteCloser struct {
	io.Writer
}

func (nopWriteCloser) Close() error { return nil }

func (r *boundedFrameReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		if r.terminalErr != nil {
			return 0, r.terminalErr
		}
		return 0, nil
	}
	if r.off < len(r.pending) {
		n := copy(p, r.pending[r.off:])
		r.off += n
		if r.off == len(r.pending) {
			r.pending = nil
			r.off = 0
		}
		return n, nil
	}
	if r.terminalErr != nil {
		return 0, r.terminalErr
	}
	if r.done {
		r.terminalErr = io.EOF
		return 0, io.EOF
	}

	frame, err := r.readFrame()
	if err != nil {
		if errors.Is(err, io.EOF) {
			r.done = true
			r.terminalErr = io.EOF
		} else {
			r.terminalErr = err
		}
		return 0, err
	}
	r.pending = frame
	n := copy(p, frame)
	r.off = n
	if r.off == len(frame) {
		r.pending = nil
		r.off = 0
	}
	return n, nil
}

// readFrame returns the next frame with a normalized LF terminator. It does
// not read beyond the physical line except for bufio's fixed-size read-ahead,
// so an oversized frame is rejected as soon as the limit is crossed.
func (r *boundedFrameReader) readFrame() ([]byte, error) {
	var frame []byte
	for {
		fragment, err := r.br.ReadSlice('\n')
		if err == nil {
			// The fragment ends with LF. Remove the LF and, when present,
			// the CR immediately before it. The CR may be at the end of a
			// previous fragment when the line crosses the bufio buffer
			// boundary.
			payload := fragment[:len(fragment)-1]
			if len(payload) > 0 && payload[len(payload)-1] == '\r' {
				payload = payload[:len(payload)-1]
			} else if len(payload) == 0 && len(frame) > 0 && frame[len(frame)-1] == '\r' {
				frame = frame[:len(frame)-1]
			}
			if len(frame)+len(payload) > r.max {
				return nil, fmt.Errorf("%w: limit %d bytes", errRequestTooLarge, r.max)
			}
			frame = append(frame, payload...)
			if len(frame) > 0 && frame[len(frame)-1] == '\r' {
				return nil, fmt.Errorf("%w: invalid CR framing", errMalformedFrame)
			}
			if !json.Valid(frame) {
				return nil, fmt.Errorf("%w: frame is not a single complete JSON value", errMalformedFrame)
			}
			return append(frame, '\n'), nil
		}

		// Without a delimiter, allow at most one trailing CR beyond the
		// configured limit. It may turn out to be the first half of a CRLF
		// terminator, and the next fragment may bring only the LF.
		if len(frame)+len(fragment) > r.max+1 {
			return nil, fmt.Errorf("%w: limit %d bytes", errRequestTooLarge, r.max)
		}
		frame = append(frame, fragment...)
		if len(frame) > r.max && frame[len(frame)-1] != '\r' {
			return nil, fmt.Errorf("%w: limit %d bytes", errRequestTooLarge, r.max)
		}

		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if len(frame) == 0 {
				return nil, io.EOF
			}
			if frame[len(frame)-1] == '\r' {
				if len(frame) > r.max {
					return nil, fmt.Errorf("%w: limit %d bytes", errRequestTooLarge, r.max)
				}
				return nil, fmt.Errorf("%w: invalid CR framing", errMalformedFrame)
			}
			if !json.Valid(frame) {
				return nil, fmt.Errorf("%w: frame is not a single complete JSON value", errMalformedFrame)
			}
			return append(frame, '\n'), nil
		default:
			return nil, err
		}
	}
}

func (r *boundedFrameReader) Close() error {
	r.closeOnce.Do(func() {
		r.closeErr = r.r.Close()
	})
	return r.closeErr
}
