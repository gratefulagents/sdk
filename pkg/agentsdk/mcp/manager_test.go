package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestNormalizeInputSchema(t *testing.T) {
	t.Parallel()

	defaultSchema := map[string]any{
		"type":                 "object",
		"properties":           map[string]any{},
		"additionalProperties": true,
	}

	tests := []struct {
		name   string
		schema any
		want   map[string]any
	}{
		{
			name:   "nil schema falls back to default",
			schema: nil,
			want:   defaultSchema,
		},
		{
			name:   "non-object junk falls back to default",
			schema: "not a schema",
			want:   defaultSchema,
		},
		{
			name: "object with type and properties passes through",
			schema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"a": map[string]any{"type": "string"}},
				"required":   []any{"a"},
			},
			want: map[string]any{
				"type":       "object",
				"properties": map[string]any{"a": map[string]any{"type": "string"}},
				"required":   []any{"a"},
			},
		},
		{
			name: "object missing type but with properties gains type",
			schema: map[string]any{
				"properties": map[string]any{"a": map[string]any{"type": "string"}},
			},
			want: map[string]any{
				"type":       "object",
				"properties": map[string]any{"a": map[string]any{"type": "string"}},
			},
		},
		{
			name: "object with type but no properties gains empty properties",
			schema: map[string]any{
				"type":                 "object",
				"additionalProperties": false,
			},
			want: map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"additionalProperties": false,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			raw := normalizeInputSchema(tt.schema)
			var got map[string]any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("normalized schema is not a JSON object: %v (raw=%s)", err, raw)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("normalizeInputSchema() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestIsSessionClosedErr(t *testing.T) {
	t.Parallel()

	closed := []error{
		mcpsdk.ErrConnectionClosed,
		fmt.Errorf("calling tool: %w", mcpsdk.ErrConnectionClosed),
		io.EOF,
		io.ErrClosedPipe,
		errors.New("write |1: broken pipe"),
	}
	for _, err := range closed {
		if !isSessionClosedErr(err) {
			t.Errorf("isSessionClosedErr(%v) = false, want true", err)
		}
	}

	notClosed := []error{
		nil,
		errors.New("tool returned an error"),
		context.DeadlineExceeded,
	}
	for _, err := range notClosed {
		if isSessionClosedErr(err) {
			t.Errorf("isSessionClosedErr(%v) = true, want false", err)
		}
	}
}

func TestShouldAttemptReconnect(t *testing.T) {
	t.Parallel()

	now := time.Now()
	tests := []struct {
		name        string
		lastAttempt time.Time
		want        bool
	}{
		{"never attempted", time.Time{}, true},
		{"within cooldown", now.Add(-time.Second), false},
		{"exactly at cooldown", now.Add(-reconnectCooldown), true},
		{"past cooldown", now.Add(-reconnectCooldown - time.Second), true},
	}
	for _, tt := range tests {
		if got := shouldAttemptReconnect(tt.lastAttempt, now, reconnectCooldown); got != tt.want {
			t.Errorf("%s: shouldAttemptReconnect() = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestReconnectServerRateLimited(t *testing.T) {
	t.Parallel()

	failed := &serverConn{name: "srv"}
	m := &Manager{
		servers:    map[string]*serverConn{"srv": failed},
		reconnects: map[string]*reconnectState{"srv": {lastAttempt: time.Now()}},
	}

	_, err := m.reconnectServer(context.Background(), "srv", failed)
	if err == nil || !strings.Contains(err.Error(), "too recently") {
		t.Fatalf("reconnectServer() error = %v, want cooldown error", err)
	}
}

func TestReconnectServerReusesConcurrentReplacement(t *testing.T) {
	t.Parallel()

	failed := &serverConn{name: "srv"}
	replacement := &serverConn{name: "srv"}
	m := &Manager{
		servers:    map[string]*serverConn{"srv": replacement},
		reconnects: map[string]*reconnectState{},
	}

	got, err := m.reconnectServer(context.Background(), "srv", failed)
	if err != nil {
		t.Fatalf("reconnectServer() error = %v", err)
	}
	if got != replacement {
		t.Fatalf("reconnectServer() = %p, want existing replacement %p", got, replacement)
	}
}

func TestReconnectServerUnknownServer(t *testing.T) {
	t.Parallel()

	m := &Manager{
		servers:    map[string]*serverConn{},
		reconnects: map[string]*reconnectState{},
	}

	_, err := m.reconnectServer(context.Background(), "gone", &serverConn{name: "gone"})
	if err == nil || !strings.Contains(err.Error(), "no longer registered") {
		t.Fatalf("reconnectServer() error = %v, want unregistered error", err)
	}
}
