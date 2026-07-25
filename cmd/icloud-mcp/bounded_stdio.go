package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

const maxStdioFrameBytes = 1 << 20

const maxMCPErrorFrameBytes = 256 << 10

const maxReflectedProtocolErrorBytes = 64 << 10

// maxJSONRPCIDBytes bounds reflected request ids on replacement error frames
// so a huge client id cannot defeat the stdout byte budget.
const maxJSONRPCIDBytes = 512

var errStdioFrameTooLarge = errors.New("stdio JSON-RPC frame exceeds 1 MiB")

// boundedFrameReader withholds a frame until its terminating newline has been
// seen. Oversized input is therefore never partially delivered to mcp-go and
// cannot be reflected by its JSON parser or protocol error handling.
type boundedFrameReader struct {
	reader  *bufio.Reader
	pending []byte
	nextErr error
}

// boundedErrorWriter covers protocol and schema-validation errors produced by
// mcp-go before tool middleware runs and redacts every complete output frame.
type boundedErrorWriter struct {
	writer   io.Writer
	redactor *security.Redactor
	mu       sync.Mutex
	pending  []byte
}

func newBoundedFrameReader(reader io.Reader) *boundedFrameReader {
	return &boundedFrameReader{reader: bufio.NewReaderSize(reader, 32<<10)}
}

func newBoundedErrorWriter(writer io.Writer, redactor *security.Redactor) *boundedErrorWriter {
	return &boundedErrorWriter{writer: writer, redactor: redactor}
}

func (r *boundedFrameReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.pending) == 0 {
		if r.nextErr != nil {
			err := r.nextErr
			r.nextErr = nil
			return 0, err
		}
		frame, err := r.readFrame()
		if err != nil {
			return 0, err
		}
		r.pending = frame
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *boundedFrameReader) readFrame() ([]byte, error) {
	frame := make([]byte, 0, 32<<10)
	contentBytes := 0
	for {
		fragment, err := r.reader.ReadSlice('\n')
		fragmentContent := len(fragment)
		if len(fragment) > 0 && fragment[len(fragment)-1] == '\n' {
			fragmentContent--
		}
		if contentBytes > maxStdioFrameBytes-fragmentContent {
			return nil, errStdioFrameTooLarge
		}
		contentBytes += fragmentContent
		frame = append(frame, fragment...)

		switch {
		case err == nil:
			return frame, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(frame) > 0:
			r.nextErr = io.EOF
			return frame, nil
		default:
			return nil, err
		}
	}
}

func (w *boundedErrorWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending)+len(p) > maxStdioFrameBytes+1 {
		// Cap pending without a newline so a huge blob cannot grow forever.
		w.pending = nil
		frame := oversizedProtocolErrorFrame(json.RawMessage("null"), "MCP response exceeded its safe byte limit")
		frame = []byte(w.redactor.Redact(string(frame)))
		if _, err := w.writer.Write(frame); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	w.pending = append(w.pending, p...)
	for {
		newline := bytes.IndexByte(w.pending, '\n')
		if newline < 0 {
			return len(p), nil
		}
		frame := append([]byte(nil), w.pending[:newline+1]...)
		w.pending = w.pending[newline+1:]
		frame = sanitizeOutputFrame(frame)
		frame = []byte(w.redactor.Redact(string(frame)))
		// Redaction can expand a frame, so re-apply caps and redact again.
		frame = sanitizeOutputFrame(frame)
		frame = []byte(w.redactor.Redact(string(frame)))
		for len(frame) > 0 {
			written, err := w.writer.Write(frame)
			if err != nil {
				return 0, err
			}
			if written <= 0 {
				return 0, io.ErrShortWrite
			}
			frame = frame[written:]
		}
	}
}

// sanitizeOutputFrame enforces the reflected-error threshold and the absolute
// stdout frame budget. Caller-reflecting protocol/tool errors are replaced
// above 64 KiB. Any remaining frame above 256 KiB is replaced so a missed
// domain result cap cannot blow the stdio channel.
func sanitizeOutputFrame(frame []byte) []byte {
	if len(frame) <= maxReflectedProtocolErrorBytes {
		return frame
	}
	type envelope struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   json.RawMessage `json:"error"`
		Result  json.RawMessage `json:"result"`
	}
	var message envelope
	if err := json.Unmarshal(bytes.TrimSpace(frame), &message); err != nil {
		if len(frame) > maxMCPErrorFrameBytes {
			return oversizedProtocolErrorFrame(json.RawMessage("null"), "MCP response exceeded its safe byte limit")
		}
		return frame
	}
	id := boundJSONRPCID(message.ID)
	if len(message.Error) > 0 && string(message.Error) != "null" {
		return oversizedProtocolErrorFrame(id, "MCP error response exceeded its safe byte limit")
	}
	var result struct {
		IsError bool `json:"isError"`
	}
	if len(message.Result) > 0 && json.Unmarshal(message.Result, &result) == nil && result.IsError {
		return oversizedToolErrorFrame(id)
	}
	if len(frame) > maxMCPErrorFrameBytes {
		return oversizedProtocolErrorFrame(id, "MCP response exceeded its safe byte limit")
	}
	return frame
}

func boundJSONRPCID(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	if len(id) > maxJSONRPCIDBytes {
		return json.RawMessage("null")
	}
	// Accept only JSON string or number ids; anything else becomes null.
	trimmed := bytes.TrimSpace(id)
	if len(trimmed) == 0 {
		return json.RawMessage("null")
	}
	switch trimmed[0] {
	case '"', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return trimmed
	default:
		return json.RawMessage("null")
	}
}

func oversizedProtocolErrorFrame(id json.RawMessage, message string) []byte {
	id = boundJSONRPCID(id)
	replacement, _ := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{
		JSONRPC: "2.0",
		ID:      id,
		Error: struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}{Code: -32603, Message: message},
	})
	if len(replacement) > maxMCPErrorFrameBytes {
		replacement, _ = json.Marshal(struct {
			JSONRPC string `json:"jsonrpc"`
			ID      any    `json:"id"`
			Error   struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}{
			JSONRPC: "2.0",
			ID:      nil,
			Error: struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			}{Code: -32603, Message: message},
		})
	}
	return append(replacement, '\n')
}

func oversizedToolErrorFrame(id json.RawMessage) []byte {
	id = boundJSONRPCID(id)
	replacement, _ := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  struct {
			Content []map[string]string `json:"content"`
			IsError bool                `json:"isError"`
		} `json:"result"`
	}{
		JSONRPC: "2.0",
		ID:      id,
		Result: struct {
			Content []map[string]string `json:"content"`
			IsError bool                `json:"isError"`
		}{
			Content: []map[string]string{{
				"type": "text",
				"text": `{"code":"payload_too_large","message":"MCP error result exceeded its safe byte limit"}`,
			}},
			IsError: true,
		},
	})
	return append(replacement, '\n')
}

func serveBoundedStdio(mcpServer *server.MCPServer, errLogger *log.Logger, redactor *security.Redactor) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return listenBoundedStdio(ctx, mcpServer, os.Stdin, os.Stdout, errLogger, redactor)
}

func listenBoundedStdio(ctx context.Context, mcpServer *server.MCPServer, stdin io.Reader, stdout io.Writer, errLogger *log.Logger, redactor *security.Redactor) error {
	stdioServer := server.NewStdioServer(mcpServer)
	stdioServer.SetErrorLogger(errLogger)
	return stdioServer.Listen(ctx, newBoundedFrameReader(stdin), newBoundedErrorWriter(stdout, redactor))
}
