package lsp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const maxLSPHeaderBytes = 8 << 10

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type rpcOutboundMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  any             `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func writeRPCMessage(w io.Writer, message any) error {
	return writeBoundedRPCMessage(w, message, 0)
}

func writeBoundedRPCMessage(w io.Writer, message any, maxBytes int) error {
	body, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if maxBytes > 0 && len(body) > maxBytes {
		return fmt.Errorf("outbound LSP message exceeds %d bytes", maxBytes)
	}
	if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

func readRPCMessage(r *bufio.Reader, maxMessageBytes int) (rpcMessage, error) {
	var contentLength = -1
	headerBytes := 0
	for {
		line, err := readLSPHeaderLine(r, maxLSPHeaderBytes-headerBytes)
		if err != nil {
			return rpcMessage{}, err
		}
		headerBytes += len(line)
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return rpcMessage{}, fmt.Errorf("invalid LSP message header %q", line)
		}
		if strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			length, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil || length < 0 {
				return rpcMessage{}, fmt.Errorf("invalid LSP Content-Length %q", value)
			}
			contentLength = length
		}
	}
	if contentLength < 0 {
		return rpcMessage{}, fmt.Errorf("LSP message has no Content-Length")
	}
	if contentLength > maxMessageBytes {
		return rpcMessage{}, fmt.Errorf("LSP message exceeds %d bytes", maxMessageBytes)
	}
	body := make([]byte, contentLength)
	if _, err := io.ReadFull(r, body); err != nil {
		return rpcMessage{}, err
	}
	var message rpcMessage
	if err := json.Unmarshal(body, &message); err != nil {
		return rpcMessage{}, fmt.Errorf("decoding LSP message: %w", err)
	}
	return message, nil
}

func readLSPHeaderLine(r *bufio.Reader, limit int) (string, error) {
	var line []byte
	for {
		fragment, err := r.ReadSlice('\n')
		if len(line)+len(fragment) > limit {
			return "", fmt.Errorf("LSP message header exceeds %d bytes", maxLSPHeaderBytes)
		}
		line = append(line, fragment...)
		if err == nil {
			return string(line), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return "", err
	}
}
