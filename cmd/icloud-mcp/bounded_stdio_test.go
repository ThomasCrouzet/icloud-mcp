package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/server"

	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

func TestBoundedFrameReaderExactAndCapPlusOne(t *testing.T) {
	exact := strings.Repeat("x", maxStdioFrameBytes) + "\n"
	got, err := io.ReadAll(newBoundedFrameReader(strings.NewReader(exact)))
	if err != nil || string(got) != exact {
		t.Fatalf("exact frame: bytes=%d err=%v", len(got), err)
	}

	tooLarge := strings.Repeat("x", maxStdioFrameBytes+1) + "\n"
	got, err = io.ReadAll(newBoundedFrameReader(strings.NewReader(tooLarge)))
	if !errors.Is(err, errStdioFrameTooLarge) || len(got) != 0 {
		t.Fatalf("cap+1 frame: bytes=%d err=%v", len(got), err)
	}
}

func TestOversizedStdioFrameIsNotReflected(t *testing.T) {
	const sentinel = "OVERSIZED-ATTACKER-SENTINEL"
	input := strings.Repeat("x", maxStdioFrameBytes) + sentinel + "\n"
	var stdout, logs bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := listenBoundedStdio(
		ctx,
		server.NewMCPServer("test", "test"),
		strings.NewReader(input),
		&stdout,
		log.New(&logs, "", 0),
		security.NewRedactor("unused-secret"),
	)
	if !errors.Is(err, errStdioFrameTooLarge) {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(stdout.String(), sentinel) || strings.Contains(logs.String(), sentinel) {
		t.Fatalf("oversized input was reflected: stdout=%q logs=%q", stdout.String(), logs.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("oversized input produced protocol output: %q", stdout.String())
	}
}

func TestBoundedErrorWriterReplacesOversizedToolError(t *testing.T) {
	const sentinel = "LARGE-ERROR-SENTINEL"
	frame, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      7,
		"result": map[string]any{
			"content": []map[string]string{{"type": "text", "text": sentinel + strings.Repeat("x", maxMCPErrorFrameBytes)}},
			"isError": true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	frame = append(frame, '\n')
	var output bytes.Buffer
	if _, err := newBoundedErrorWriter(&output, security.NewRedactor("unused-secret")).Write(frame); err != nil {
		t.Fatal(err)
	}
	if output.Len() > maxMCPErrorFrameBytes || strings.Contains(output.String(), sentinel) {
		t.Fatalf("bounded output length=%d reflected=%t", output.Len(), strings.Contains(output.String(), sentinel))
	}
	var response struct {
		Result struct {
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &response); err != nil || !response.Result.IsError {
		t.Fatalf("replacement is not a valid tool error: response=%+v err=%v", response, err)
	}
}

func TestBoundedErrorWriterRedactsEveryFrameBeforeEmission(t *testing.T) {
	const secret = "FRAMEWORK-SECRET-SENTINEL"
	frames := []string{
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"invalid value FRAMEWORK-SECRET-SENTINEL"}}` + "\n",
		`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"FRAMEWORK-SECRET-SENTINEL"}]}}` + "\n",
	}
	for _, frame := range frames {
		if len(frame) >= maxReflectedProtocolErrorBytes {
			t.Fatalf("test frame length = %d, want below reflected-error threshold", len(frame))
		}
		var output bytes.Buffer
		writer := newBoundedErrorWriter(&output, security.NewRedactor(secret))
		mid := len(frame) / 2
		if _, err := writer.Write([]byte(frame[:mid])); err != nil {
			t.Fatal(err)
		}
		if output.Len() != 0 {
			t.Fatalf("partial frame emitted early: %q", output.String())
		}
		if _, err := writer.Write([]byte(frame[mid:])); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(output.String(), secret) || !strings.Contains(output.String(), "[REDACTED]") {
			t.Fatalf("frame was not redacted: %q", output.String())
		}
		if !json.Valid(bytes.TrimSpace(output.Bytes())) {
			t.Fatalf("redacted frame is invalid JSON: %q", output.String())
		}
	}
}

func TestBoundedErrorWriterReappliesCapAfterRedactionExpansion(t *testing.T) {
	const secret = "abcd"
	frame, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      9,
		"error": map[string]any{
			"code":    -32602,
			"message": strings.Repeat(secret, (maxReflectedProtocolErrorBytes-1024)/len(secret)),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	frame = append(frame, '\n')
	if len(frame) > maxReflectedProtocolErrorBytes {
		t.Fatalf("input frame length = %d, want at most %d", len(frame), maxReflectedProtocolErrorBytes)
	}

	var output bytes.Buffer
	if _, err := newBoundedErrorWriter(&output, security.NewRedactor(secret)).Write(frame); err != nil {
		t.Fatal(err)
	}
	if output.Len() > maxReflectedProtocolErrorBytes {
		t.Fatalf("output frame length = %d, cap = %d", output.Len(), maxReflectedProtocolErrorBytes)
	}
	if strings.Contains(output.String(), secret) {
		t.Fatalf("expanded replacement frame leaked secret: %q", output.String())
	}
	var response struct {
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &response); err != nil || response.Error.Code != -32603 {
		t.Fatalf("response=%+v err=%v", response, err)
	}
}
