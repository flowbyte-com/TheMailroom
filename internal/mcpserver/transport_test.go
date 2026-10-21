package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func frameOfSize(t *testing.T, n int, method string) []byte {
	t.Helper()
	prefix := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":{"pad":"`, method)
	suffix := `"}}`
	if n < len(prefix)+len(suffix) {
		t.Fatalf("frame size %d is less than minimum %d", n, len(prefix)+len(suffix))
	}
	frame := make([]byte, 0, n)
	frame = append(frame, prefix...)
	frame = append(frame, bytes.Repeat([]byte("a"), n-len(prefix)-len(suffix))...)
	frame = append(frame, suffix...)
	if len(frame) != n {
		t.Fatalf("frame length = %d, want %d", len(frame), n)
	}
	if !json.Valid(frame) {
		t.Fatalf("generated frame is not valid JSON")
	}
	return frame
}

func withTerminator(frame []byte, terminator string) []byte {
	out := append([]byte(nil), frame...)
	return append(out, terminator...)
}

func connectBounded(t *testing.T, input []byte, max int) (mcp.Connection, *lockedBuffer) {
	t.Helper()
	out := &lockedBuffer{}
	conn := connectTransport(t, newBoundedTransport(io.NopCloser(bytes.NewReader(input)), out, max))
	return conn, out
}

func connectTransport(t *testing.T, transport mcp.Transport) mcp.Connection {
	t.Helper()
	conn, err := transport.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func readWithin(t *testing.T, conn mcp.Connection) (jsonrpc.Message, error) {
	t.Helper()
	return readFor(t, conn, 2*time.Second)
}

func readFor(t *testing.T, conn mcp.Connection, timeout time.Duration) (jsonrpc.Message, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return conn.Read(ctx)
}

func TestBoundedTransportExactLimit(t *testing.T) {
	const max = 512
	frame := frameOfSize(t, max, "test")
	tests := []struct {
		name  string
		input []byte
	}{
		{name: "LF", input: withTerminator(frame, "\n")},
		{name: "CRLF", input: withTerminator(frame, "\r\n")},
		{name: "EOF without terminator", input: withTerminator(frame, "")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn, _ := connectBounded(t, tc.input, max)
			msg, err := readWithin(t, conn)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if msg == nil {
				t.Fatal("Read returned nil message")
			}
			if _, err := readWithin(t, conn); !errors.Is(err, io.EOF) {
				t.Errorf("second Read error = %v, want io.EOF", err)
			}
		})
	}
}

func TestBoundedTransportCRLFAtBufferBoundary(t *testing.T) {
	// bufio.Reader's default buffer is 4096 bytes. These cases put the CR
	// at the buffer boundary, split CRLF into a later fragment, and put the
	// configured limit immediately before the CRLF terminator.
	for _, tc := range []struct {
		max int
		n   int
	}{
		{max: 4095, n: 4095},
		{max: 4096, n: 4095},
		{max: 4096, n: 4096},
	} {
		t.Run(fmt.Sprintf("max_%d_payload_%d", tc.max, tc.n), func(t *testing.T) {
			frame := frameOfSize(t, tc.n, "test")
			conn, _ := connectBounded(t, withTerminator(frame, "\r\n"), tc.max)
			if _, err := readWithin(t, conn); err != nil {
				t.Fatalf("Read: %v", err)
			}
		})
	}

	t.Run("max_4095_payload_4096", func(t *testing.T) {
		frame := frameOfSize(t, 4096, "test")
		conn, _ := connectBounded(t, withTerminator(frame, "\r\n"), 4095)
		if _, err := readWithin(t, conn); !errors.Is(err, errRequestTooLarge) {
			t.Fatalf("Read error = %v, want errRequestTooLarge", err)
		}
	})
}

func TestBoundedTransportExceedsLimit(t *testing.T) {
	const max = 512
	validOversize := frameOfSize(t, max+1, "read_message")
	invalidOversize := append(bytes.Repeat([]byte("{"), max+1), '\n')
	tests := []struct {
		name  string
		input []byte
	}{
		{name: "valid LF", input: withTerminator(validOversize, "\n")},
		{name: "valid CRLF", input: withTerminator(validOversize, "\r\n")},
		{name: "valid EOF", input: withTerminator(validOversize, "")},
		{name: "invalid JSON oversized", input: invalidOversize},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn, _ := connectBounded(t, tc.input, max)
			_, err := readWithin(t, conn)
			if !errors.Is(err, errRequestTooLarge) {
				t.Fatalf("Read error = %v, want errRequestTooLarge", err)
			}
			if strings.Contains(err.Error(), strings.Repeat("a", 64)) {
				t.Error("error discloses request payload")
			}
		})
	}
}

func TestBoundedTransportMalformedFrames(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
	}{
		{name: "incomplete", input: []byte(`{"jsonrpc":"2.0"`)},
		{name: "invalid syntax", input: []byte("{\"jsonrpc\":}\n")},
		{name: "multiple values on one line", input: []byte("{\"a\":1}{\"b\":2}\n")},
		{name: "blank line", input: []byte("\n")},
		{name: "multi-line JSON value", input: []byte("{\n\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"test\"}\n")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn, _ := connectBounded(t, tc.input, 512)
			_, err := readWithin(t, conn)
			if !errors.Is(err, errMalformedFrame) {
				t.Fatalf("Read error = %v, want errMalformedFrame", err)
			}
		})
	}
}

func TestBoundedTransportConsecutiveFramesAndChunking(t *testing.T) {
	const max = 512
	first := frameOfSize(t, 256, "first")
	second := frameOfSize(t, 320, "second")
	input := append(withTerminator(first, "\n"), withTerminator(second, "\r\n")...)

	out := &lockedBuffer{}
	reader := &chunkedReader{r: bytes.NewReader(input), chunk: 1}
	conn := connectTransport(t, newBoundedTransport(io.NopCloser(reader), out, max))
	for i, want := range []string{"first", "second"} {
		msg, err := readWithin(t, conn)
		if err != nil {
			t.Fatalf("Read %d: %v", i, err)
		}
		req, ok := msg.(*jsonrpc.Request)
		if !ok {
			t.Fatalf("Read %d returned %T, want *jsonrpc.Request", i, msg)
		}
		if req.Method != want {
			t.Errorf("Read %d method = %q, want %q", i, req.Method, want)
		}
	}
	if _, err := readWithin(t, conn); !errors.Is(err, io.EOF) {
		t.Errorf("final Read error = %v, want io.EOF", err)
	}
}

func TestBoundedTransportLimitAppliesToSecondFrame(t *testing.T) {
	const max = 512
	first := frameOfSize(t, max, "first")
	second := frameOfSize(t, max+1, "second")
	input := append(withTerminator(first, "\n"), withTerminator(second, "\n")...)
	conn, _ := connectBounded(t, input, max)

	if _, err := readWithin(t, conn); err != nil {
		t.Fatalf("first Read: %v", err)
	}
	if _, err := readWithin(t, conn); !errors.Is(err, errRequestTooLarge) {
		t.Fatalf("second Read error = %v, want errRequestTooLarge", err)
	}
}

func TestBoundedTransportTerminalErrorStopsDecoding(t *testing.T) {
	const max = 512
	valid := frameOfSize(t, 128, "after-error")
	cases := []struct {
		name  string
		input []byte
	}{
		{
			name:  "oversize",
			input: append(withTerminator(frameOfSize(t, max+1, "oversize"), "\n"), withTerminator(valid, "\n")...),
		},
		{
			name:  "malformed",
			input: append([]byte("{\"jsonrpc\":}\n"), withTerminator(valid, "\n")...),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn, _ := connectBounded(t, tc.input, max)
			if msg, err := readWithin(t, conn); err == nil || msg != nil {
				t.Fatal("first Read yielded a message, want terminal framing error")
			}
			if msg, _ := readFor(t, conn, 100*time.Millisecond); msg != nil {
				t.Fatal("second Read yielded a message after terminal framing error")
			}
		})
	}
}

func TestBoundedTransportBatch(t *testing.T) {
	const batch = `[{"jsonrpc":"2.0","id":1,"method":"first"},{"jsonrpc":"2.0","id":2,"method":"second"}]`
	conn, _ := connectBounded(t, append([]byte(batch), '\n'), 512)

	for i, want := range []string{"first", "second"} {
		msg, err := readWithin(t, conn)
		if err != nil {
			t.Fatalf("Read %d: %v", i, err)
		}
		req, ok := msg.(*jsonrpc.Request)
		if !ok {
			t.Fatalf("Read %d returned %T, want *jsonrpc.Request", i, msg)
		}
		if req.Method != want {
			t.Errorf("Read %d method = %q, want %q", i, req.Method, want)
		}
	}
	if _, err := readWithin(t, conn); !errors.Is(err, io.EOF) {
		t.Errorf("final Read error = %v, want io.EOF", err)
	}
}

func TestBoundedTransportNotificationsAndUnknownMethods(t *testing.T) {
	input := []byte(
		`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
			`{"jsonrpc":"2.0","id":3,"method":"unknown/method"}` + "\n",
	)
	conn, _ := connectBounded(t, input, 512)
	for i, want := range []string{"notifications/initialized", "unknown/method"} {
		msg, err := readWithin(t, conn)
		if err != nil {
			t.Fatalf("Read %d: %v", i, err)
		}
		req, ok := msg.(*jsonrpc.Request)
		if !ok {
			t.Fatalf("Read %d returned %T, want *jsonrpc.Request", i, msg)
		}
		if req.Method != want {
			t.Errorf("Read %d method = %q, want %q", i, req.Method, want)
		}
	}
}

func TestBoundedTransportCleanEOF(t *testing.T) {
	conn, _ := connectBounded(t, nil, 512)
	if _, err := readWithin(t, conn); !errors.Is(err, io.EOF) {
		t.Fatalf("Read error = %v, want io.EOF", err)
	}
}

func TestBoundedTransportCloseUnblocksRead(t *testing.T) {
	reader := newSignalingReadCloser()
	conn := connectTransport(t, newBoundedTransport(reader, io.Discard, 512))
	readErr := make(chan error, 1)
	go func() {
		_, err := conn.Read(context.Background())
		readErr <- err
	}()

	select {
	case <-reader.started:
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not start")
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-readErr:
		if err == nil {
			t.Fatal("Read returned nil error after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not unblock after Close")
	}
}

func TestBoundedTransportContextCancellation(t *testing.T) {
	reader := newSignalingReadCloser()
	conn := connectTransport(t, newBoundedTransport(reader, io.Discard, 512))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	readErr := make(chan error, 1)
	go func() {
		_, err := conn.Read(ctx)
		readErr <- err
	}()

	select {
	case <-reader.started:
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not start")
	}
	cancel()
	select {
	case err := <-readErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Read error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not return after context cancellation")
	}
}

func TestBoundedServerRunCancellation(t *testing.T) {
	reader := newSignalingReadCloser()
	server, _ := newTestMCPServer(t)
	serverTransport := newBoundedTransport(reader, io.Discard, 512)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		_ = reader.Close()
	}()
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.Run(ctx, serverTransport)
	}()

	select {
	case <-reader.started:
	case <-time.After(2 * time.Second):
		t.Fatal("transport read did not start")
	}
	cancel()
	select {
	case err := <-serverErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("server Run error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server Run did not stop after context cancellation")
	}
}

func TestBoundedTransportConcurrentWrites(t *testing.T) {
	out := &lockedBuffer{}
	conn := connectTransport(t, newBoundedTransport(io.NopCloser(bytes.NewReader(nil)), out, 512))

	const writes = 32
	errs := make(chan error, writes)
	var wg sync.WaitGroup
	for i := 0; i < writes; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- conn.Write(context.Background(), &jsonrpc.Request{
				Method: "test",
				Params: json.RawMessage(`{"n":1}`),
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != writes {
		t.Fatalf("wrote %d lines, want %d", len(lines), writes)
	}
	for i, line := range lines {
		if !json.Valid([]byte(line)) {
			t.Errorf("line %d is not valid JSON: %q", i, line)
		}
	}
}

func TestBoundedServerRejectsOversizeBeforeToolHandler(t *testing.T) {
	const max = 512
	server, calls := newTestMCPServer(t)
	inputReader, inputWriter := io.Pipe()
	var output lockedBuffer
	serverTransport := newBoundedTransport(inputReader, &output, max)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.Run(ctx, serverTransport)
	}()

	frame := frameOfSize(t, max+1, "send_message")
	writeErr := make(chan error, 1)
	go func() {
		_, err := inputWriter.Write(withTerminator(frame, "\n"))
		writeErr <- err
	}()

	select {
	case err := <-serverErr:
		if err == nil {
			t.Fatal("server Run returned nil after oversized frame")
		}
		if !errors.Is(err, errRequestTooLarge) {
			t.Errorf("server Run error = %v, want errRequestTooLarge", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not reject oversized frame")
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("tool handler invoked %d times, want 0", got)
	}
	select {
	case <-writeErr:
	case <-time.After(2 * time.Second):
		t.Fatal("input write did not return after rejection")
	}

	for i, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		if line == "" {
			continue
		}
		if !json.Valid([]byte(line)) {
			t.Errorf("stdout line %d is not valid JSON: %q", i, line)
		}
	}
	if strings.Contains(output.String(), strings.Repeat("a", 64)) {
		t.Error("stdout contains oversized request payload")
	}
}

func TestBoundedServerNormalInitializationAndToolCall(t *testing.T) {
	server, calls := newTestMCPServer(t)
	serverTransport, clientTransport, cleanup := newPipeTransportPair(4096)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.Run(ctx, serverTransport)
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "bounded-test-client", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "send_message",
		Arguments: map[string]any{"text": "hello"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("tool handler invoked %d times, want 1", got)
	}
	if len(result.Content) != 1 {
		t.Fatalf("len(Content) = %d, want 1", len(result.Content))
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("Content[0] = %T, want *mcp.TextContent", result.Content[0])
	}
	if text.Text != "hello" {
		t.Errorf("Content[0].Text = %q, want hello", text.Text)
	}

	_ = session.Close()
	select {
	case err := <-serverErr:
		if err != nil {
			t.Errorf("server Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop after client close")
	}
}

func TestBoundedServerLegacyBatch(t *testing.T) {
	const max = 4096
	server, _ := newTestMCPServer(t)
	clientToServerR, clientToServerW := io.Pipe()
	serverToClientR, serverToClientW := io.Pipe()
	serverTransport := newBoundedTransport(clientToServerR, serverToClientW, max)
	reader := bufio.NewReader(serverToClientR)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.Run(ctx, serverTransport)
	}()
	defer func() {
		_ = clientToServerW.Close()
		_ = serverToClientR.Close()
		_ = serverToClientW.Close()
		_ = clientToServerR.Close()
	}()

	writeLine(t, clientToServerW, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"raw-test","version":"1"}}}`)
	initLine := readLine(t, reader)
	var initResponse struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if err := json.Unmarshal(initLine, &initResponse); err != nil {
		t.Fatalf("initialize response: %v (line %s)", err, initLine)
	}
	if initResponse.Result.ProtocolVersion != "2025-03-26" {
		t.Fatalf("negotiated protocol = %q, want 2025-03-26", initResponse.Result.ProtocolVersion)
	}

	writeLine(t, clientToServerW, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	writeLine(t, clientToServerW, `[{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}},{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}]`)
	batchLine := readLine(t, reader)
	var responses []json.RawMessage
	if err := json.Unmarshal(batchLine, &responses); err != nil {
		t.Fatalf("batch response is not an array: %v (line %s)", err, batchLine)
	}
	if len(responses) != 2 {
		t.Fatalf("batch response length = %d, want 2", len(responses))
	}
	seen := map[float64]bool{}
	for _, raw := range responses {
		var response struct {
			ID float64 `json:"id"`
		}
		if err := json.Unmarshal(raw, &response); err != nil {
			t.Fatalf("batch response element: %v", err)
		}
		seen[response.ID] = true
	}
	if !seen[2] || !seen[3] {
		t.Errorf("batch response IDs = %v, want 2 and 3", seen)
	}

	_ = clientToServerW.Close()
	select {
	case err := <-serverErr:
		if err != nil {
			t.Errorf("server Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop after client close")
	}
}

func writeLine(t *testing.T, w io.Writer, line string) {
	t.Helper()
	errCh := make(chan error, 1)
	go func() {
		_, err := io.WriteString(w, line+"\n")
		errCh <- err
	}()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("write line: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("write line timed out")
	}
}

func readLine(t *testing.T, r *bufio.Reader) []byte {
	t.Helper()
	type result struct {
		line []byte
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		line, err := r.ReadBytes('\n')
		resultCh <- result{line: line, err: err}
	}()
	select {
	case res := <-resultCh:
		if res.err != nil {
			t.Fatalf("read line: %v", res.err)
		}
		return res.line
	case <-time.After(2 * time.Second):
		t.Fatal("read line timed out")
		return nil
	}
}

func newTestMCPServer(t *testing.T) (*mcp.Server, *atomic.Int32) {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "bounded-test-server", Version: "1"}, nil)
	var calls atomic.Int32
	mcp.AddTool(server, &mcp.Tool{Name: "send_message"}, func(
		_ context.Context,
		_ *mcp.CallToolRequest,
		input struct {
			Text string `json:"text"`
		},
	) (*mcp.CallToolResult, any, error) {
		calls.Add(1)
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: input.Text}},
		}, nil, nil
	})
	return server, &calls
}

// boundedServer drives a server over a bounded transport backed by pipes and
// exposes raw frame helpers, so tests can assert on exact wire behaviour.
type boundedServer struct {
	write func(frame string)
	read  func() []byte
	// stop closes the client side and waits for the server to return.
	stop func() error
}

func startBoundedServer(t *testing.T, server *mcp.Server, max int) *boundedServer {
	t.Helper()

	clientToServerR, clientToServerW := io.Pipe()
	serverToClientR, serverToClientW := io.Pipe()
	transport := newBoundedTransport(clientToServerR, serverToClientW, max)
	reader := bufio.NewReader(serverToClientR)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.Run(ctx, transport)
	}()

	bs := &boundedServer{
		write: func(frame string) { writeLine(t, clientToServerW, frame) },
		read:  func() []byte { return readLine(t, reader) },
		stop: func() error {
			_ = clientToServerW.Close()
			select {
			case err := <-serverErr:
				return err
			case <-time.After(2 * time.Second):
				t.Fatal("server did not stop after client close")
				return nil
			}
		},
	}
	t.Cleanup(func() {
		_ = clientToServerW.Close()
		_ = serverToClientR.Close()
		_ = serverToClientW.Close()
		_ = clientToServerR.Close()
	})
	return bs
}

func initFrameFor(protocolVersion string) string {
	return fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":%q,"capabilities":{},"clientInfo":{"name":"raw-test","version":"1"}}}`,
		protocolVersion)
}

// TestBoundedServerProtocolVersionNegotiation pins the negotiated protocol
// version. 2024-11-05, 2025-03-26 and 2025-06-18 are the versions the SDK
// supported before the upgrade and must keep negotiating to themselves;
// 2025-11-25 is the version the upgrade adds. An unknown version falls back
// to the newest supported version.
func TestBoundedServerProtocolVersionNegotiation(t *testing.T) {
	tests := []struct {
		name      string
		requested string
		want      string
	}{
		{name: "2024-11-05", requested: "2024-11-05", want: "2024-11-05"},
		{name: "2025-03-26", requested: "2025-03-26", want: "2025-03-26"},
		{name: "2025-06-18", requested: "2025-06-18", want: "2025-06-18"},
		{name: "2025-11-25", requested: "2025-11-25", want: "2025-11-25"},
		{name: "unsupported", requested: "1999-01-01", want: "2025-11-25"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := newTestMCPServer(t)
			bs := startBoundedServer(t, server, 4096)
			bs.write(initFrameFor(tc.requested))

			line := bs.read()
			var resp struct {
				Result struct {
					ProtocolVersion string `json:"protocolVersion"`
				} `json:"result"`
				Error *struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(line, &resp); err != nil {
				t.Fatalf("initialize response: %v (line %s)", err, line)
			}
			if resp.Error != nil {
				t.Fatalf("initialize failed: code=%d message=%q", resp.Error.Code, resp.Error.Message)
			}
			if resp.Result.ProtocolVersion != tc.want {
				t.Errorf("negotiated protocol = %q, want %q", resp.Result.ProtocolVersion, tc.want)
			}
		})
	}
}

// TestBoundedServerLegacyBatchRejectedFrom20250618 documents that legacy
// JSON-RPC batching survives only for protocol versions that predate
// 2025-06-18. The SDK refuses a batch at the transport boundary for newer
// versions, so the connection is closed without a response.
func TestBoundedServerLegacyBatchRejectedFrom20250618(t *testing.T) {
	for _, version := range []string{"2025-06-18", "2025-11-25"} {
		t.Run(version, func(t *testing.T) {
			server, _ := newTestMCPServer(t)
			bs := startBoundedServer(t, server, 4096)
			bs.write(initFrameFor(version))
			bs.read() // initialize response
			bs.write(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
			bs.write(`[{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}},{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}]`)

			err := bs.stop()
			if err == nil {
				t.Fatal("server Run returned nil after a batch on protocol " + version)
			}
			if !strings.Contains(err.Error(), "batching is not supported") {
				t.Errorf("server Run error = %v, want a batching rejection", err)
			}
		})
	}
}

// toolErrorResponse is the subset of a JSON-RPC response that a client
// needs in order to read a tool execution error. It deliberately omits
// structuredContent: the SDK's SetError does not populate that field, so a
// client must not depend on it.
type toolErrorResponse struct {
	Result *struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// consumeToolError reads a tool execution error the way a client must: from
// isError and the text blocks in content, without consulting
// structuredContent. It reports whether the response was a readable tool
// execution error.
func consumeToolError(resp *toolErrorResponse) (string, bool) {
	if resp.Error != nil || resp.Result == nil || !resp.Result.IsError {
		return "", false
	}
	var text string
	for _, c := range resp.Result.Content {
		if c.Type == "text" {
			text += c.Text
		}
	}
	return text, text != ""
}

// TestBoundedServerToolValidationErrorShape pins the two error channels the
// MCP specification defines for tool calls. A schema-invalid argument is a
// tool execution error and is reported in the result with isError set; an
// unknown tool is a protocol error and is reported as a JSON-RPC error.
func TestBoundedServerToolValidationErrorShape(t *testing.T) {
	server, calls := newTestMCPServer(t)
	bs := startBoundedServer(t, server, 4096)
	bs.write(initFrameFor("2025-03-26"))
	bs.read() // initialize response
	bs.write(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	// "text" is required by the test tool's input schema.
	bs.write(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"send_message","arguments":{}}}`)
	line := bs.read()

	var resp toolErrorResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("tools/call response: %v (line %s)", err, line)
	}
	if resp.Error != nil {
		t.Fatalf("schema-invalid call returned a JSON-RPC error (code=%d, message=%q); want a tool execution error",
			resp.Error.Code, resp.Error.Message)
	}
	if resp.Result == nil {
		t.Fatal("schema-invalid call returned no result")
	}
	if !resp.Result.IsError {
		t.Error("schema-invalid call returned isError=false, want true")
	}
	if len(resp.Result.Content) != 1 || resp.Result.Content[0].Type != "text" {
		t.Fatalf("schema-invalid call content = %+v, want one text block", resp.Result.Content)
	}
	if !strings.Contains(resp.Result.Content[0].Text, "validating") {
		t.Errorf("schema-invalid call message = %q, want it to mention validation", resp.Result.Content[0].Text)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("tool handler invoked %d times, want 0", got)
	}

	// The SDK's SetError populates content and isError only. It does not
	// populate structuredContent, so a client must be able to consume the
	// error without it.
	if bytes.Contains(line, []byte(`"structuredContent"`)) {
		t.Errorf("schema-invalid call carries structuredContent: %s", line)
	}
	msg, ok := consumeToolError(&resp)
	if !ok {
		t.Fatal("schema-invalid call is not consumable from isError and content")
	}
	if !strings.Contains(msg, `"text"`) {
		t.Errorf("consumed validation message = %q, want it to name the missing argument", msg)
	}

	// An unknown tool stays a protocol error.
	bs.write(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"no_such_tool","arguments":{}}}`)
	line = bs.read()
	resp = toolErrorResponse{}
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("tools/call response: %v (line %s)", err, line)
	}
	if resp.Error == nil {
		t.Fatalf("unknown tool returned a result, want a JSON-RPC error (line %s)", line)
	}
	if resp.Error.Code != -32602 {
		t.Errorf("unknown tool error code = %d, want -32602", resp.Error.Code)
	}
	if resp.Result != nil {
		t.Errorf("unknown tool returned a result alongside the protocol error: %s", line)
	}
	if _, ok := consumeToolError(&resp); ok {
		t.Error("unknown tool was consumable as a tool execution error; want a protocol error")
	}
}

func newPipeTransportPair(max int) (server mcp.Transport, client mcp.Transport, cleanup func()) {
	clientToServerR, clientToServerW := io.Pipe()
	serverToClientR, serverToClientW := io.Pipe()
	server = newBoundedTransport(clientToServerR, serverToClientW, max)
	client = &mcp.IOTransport{Reader: serverToClientR, Writer: clientToServerW}
	cleanup = func() {
		_ = clientToServerW.Close()
		_ = serverToClientR.Close()
		_ = serverToClientW.Close()
		_ = clientToServerR.Close()
	}
	return server, client, cleanup
}

type chunkedReader struct {
	r     io.Reader
	chunk int
}

func (r *chunkedReader) Read(p []byte) (int, error) {
	if len(p) > r.chunk {
		p = p[:r.chunk]
	}
	return r.r.Read(p)
}

type signalingReadCloser struct {
	started   chan struct{}
	release   chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
}

func newSignalingReadCloser() *signalingReadCloser {
	return &signalingReadCloser{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (r *signalingReadCloser) Read([]byte) (int, error) {
	r.startOnce.Do(func() { close(r.started) })
	<-r.release
	return 0, io.ErrClosedPipe
}

func (r *signalingReadCloser) Close() error {
	r.closeOnce.Do(func() { close(r.release) })
	return nil
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
