package mcp

import (
	"context"
	"testing"
)

func TestRequestBreakGlassToolDelegatesToSink(t *testing.T) {
	sink := &recordingBreakGlassSink{}
	tool := &RequestBreakGlassTool{Sink: sink}

	result, err := tool.Execute(context.Background(), []byte(`{"server":" files ","tool":" read ","reason":" inspect docs "}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || !result.ShouldPause {
		t.Fatalf("result = %+v, want pause success", result)
	}
	if sink.request.Server != "files" || sink.request.Tool != "read" || sink.request.Reason != "inspect docs" {
		t.Fatalf("request = %+v", sink.request)
	}
}

func TestRequestBreakGlassToolValidatesServer(t *testing.T) {
	result, err := (&RequestBreakGlassTool{}).Execute(context.Background(), []byte(`{"reason":"missing"}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || result.Content != "server is required" {
		t.Fatalf("result = %+v, want server error", result)
	}
}

func TestBreakGlassCatalogValidate(t *testing.T) {
	catalog := NewBreakGlassCatalog(
		[]ToolDescriptor{{ServerName: "files", ToolName: "read"}},
		[]string{"shell"},
	)
	if err := catalog.Validate("files", "read"); err != nil {
		t.Fatalf("Validate(files/read) error = %v", err)
	}
	if err := catalog.Validate("shell", ""); err != nil {
		t.Fatalf("Validate(shell) error = %v", err)
	}
	if err := catalog.Validate("missing", ""); err == nil {
		t.Fatal("Validate(missing) error = nil, want error")
	}
	if err := catalog.Validate("files", "write"); err == nil {
		t.Fatal("Validate(files/write) error = nil, want error")
	}
}

type recordingBreakGlassSink struct {
	request BreakGlassRequest
}

func (s *recordingBreakGlassSink) RequestMCPBreakGlass(_ context.Context, request BreakGlassRequest) (BreakGlassRequestResult, error) {
	s.request = request
	return BreakGlassRequestResult{Content: "queued", ShouldPause: true}, nil
}
