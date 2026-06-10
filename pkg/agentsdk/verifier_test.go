package agentsdk

import (
	"context"
	"strings"
	"testing"
)

func TestNewCriticVerifierApprovedAndRejected(t *testing.T) {
	approveModel := &subagentToolMockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "Checked everything.\nVERDICT: APPROVED"}}}},
		},
	}
	critic := &Agent{Name: "critic"}
	verify := NewCriticVerifier(NewRunnerWithModel(approveModel), critic, "build the widget")
	feedback, err := verify(context.Background(), "widget built")
	if err != nil {
		t.Fatal(err)
	}
	if feedback != "" {
		t.Fatalf("approved verdict should yield no feedback, got %q", feedback)
	}

	rejectModel := &subagentToolMockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "VERDICT: REJECTED\n1. widget.go does not exist"}}}},
		},
	}
	verify = NewCriticVerifier(NewRunnerWithModel(rejectModel), critic, "build the widget")
	feedback, err = verify(context.Background(), "widget built")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(feedback, "widget.go does not exist") {
		t.Fatalf("expected rejection feedback, got %q", feedback)
	}
}
