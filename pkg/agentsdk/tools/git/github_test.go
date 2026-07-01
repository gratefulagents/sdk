package git

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestCreatePullRequestToolRunsGitHubFlowAndRecordsURL(t *testing.T) {
	runner := &fakeRunner{
		gitOut: map[string]string{
			"status --porcelain": " M changed.go\n?? new.go\n",
		},
		ghOut: map[string]string{
			"pr create --title Add feature --body  --base main --draft": "",
			"pr view --json url -q .url":                                "https://github.com/acme/repo/pull/7\n",
		},
	}
	sink := &fakeSink{}
	tool := NewCreatePullRequestTool(runner, sink)
	input := mustJSON(t, map[string]any{
		"title":       "Add feature",
		"base_branch": "main",
		"draft":       true,
	})

	result, err := tool.Execute(context.Background(), input, "/repo")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("Execute() returned error result: %s", result.Content)
	}
	if sink.prURL != "https://github.com/acme/repo/pull/7" {
		t.Fatalf("recorded PR URL = %q", sink.prURL)
	}
	wantGit := []string{"status --porcelain", "add -A", "commit --no-verify -m Add feature", "push --no-verify -u origin HEAD"}
	if !reflect.DeepEqual(runner.gitCalls, wantGit) {
		t.Fatalf("git calls = %#v, want %#v", runner.gitCalls, wantGit)
	}
	if !strings.Contains(result.Content, `"pr_url":"https://github.com/acme/repo/pull/7"`) {
		t.Fatalf("result content = %s", result.Content)
	}
}

func TestCreatePullRequestToolCommitsUntrackedOnlyChanges(t *testing.T) {
	runner := &fakeRunner{
		gitOut: map[string]string{
			"status --porcelain": "?? new.go\n",
		},
		ghOut: map[string]string{
			"pr create --fill":           "https://github.com/acme/repo/pull/9\n",
			"pr view --json url -q .url": "https://github.com/acme/repo/pull/9\n",
		},
	}

	result, err := NewCreatePullRequestTool(runner, nil).Execute(context.Background(), json.RawMessage(`{}`), "/repo")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("Execute() returned error result: %s", result.Content)
	}
	wantGit := []string{"status --porcelain", "add -A", "commit --no-verify -m changes from agent run", "push --no-verify -u origin HEAD"}
	if !reflect.DeepEqual(runner.gitCalls, wantGit) {
		t.Fatalf("git calls = %#v, want %#v", runner.gitCalls, wantGit)
	}
}

func TestCreatePullRequestToolUsesExistingPRWhenCreateFails(t *testing.T) {
	runner := &fakeRunner{
		ghOut: map[string]string{
			"pr view --json url -q .url": "https://github.com/acme/repo/pull/8\n",
		},
		ghErr: map[string]error{
			"pr create --fill": errors.New("already exists"),
		},
	}
	sink := &fakeSink{}
	tool := NewCreatePullRequestTool(runner, sink)

	result, err := tool.Execute(context.Background(), json.RawMessage(`{}`), "/repo")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.IsError || !strings.Contains(result.Content, "PR already exists") {
		t.Fatalf("result = %+v", result)
	}
	if sink.prURL != "https://github.com/acme/repo/pull/8" {
		t.Fatalf("recorded PR URL = %q", sink.prURL)
	}
}

func TestCreateIssueToolRunsGitHubFlowAndRecordsURL(t *testing.T) {
	runner := &fakeRunner{
		ghOut: map[string]string{
			"issue create --title Bug --body Details --label bug --label sdk --assignee octo": "https://github.com/acme/repo/issues/3\n",
		},
	}
	sink := &fakeSink{}
	tool := NewCreateIssueTool(runner, sink)
	input := mustJSON(t, map[string]any{
		"title":     "Bug",
		"body":      "Details",
		"labels":    []string{"bug", "sdk"},
		"assignees": []string{"octo"},
	})

	result, err := tool.Execute(context.Background(), input, "/repo")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("Execute() returned error result: %s", result.Content)
	}
	if sink.issueURL != "https://github.com/acme/repo/issues/3" {
		t.Fatalf("recorded issue URL = %q", sink.issueURL)
	}
	if !strings.Contains(result.Content, `"issue_url":"https://github.com/acme/repo/issues/3"`) {
		t.Fatalf("result content = %s", result.Content)
	}
}

func TestCreateIssueToolRequiresTitle(t *testing.T) {
	result, err := NewCreateIssueTool(&fakeRunner{}, nil).Execute(context.Background(), json.RawMessage(`{}`), "/repo")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content, "title is required") {
		t.Fatalf("result = %+v", result)
	}
}

func TestCreateIssueToolRejectsUnexpectedOutput(t *testing.T) {
	runner := &fakeRunner{
		ghOut: map[string]string{
			"issue create --title Bug": "not a url",
		},
	}
	result, err := NewCreateIssueTool(runner, nil).Execute(context.Background(), json.RawMessage(`{"title":"Bug"}`), "/repo")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content, "unexpected output") {
		t.Fatalf("result = %+v, want unexpected output error", result)
	}
}

type fakeRunner struct {
	gitOut   map[string]string
	ghOut    map[string]string
	gitErr   map[string]error
	ghErr    map[string]error
	gitFn    func(context.Context, string, ...string) (string, error)
	ghFn     func(context.Context, string, ...string) (string, error)
	gitCalls []string
	ghCalls  []string
	gitDirs  []string
	ghDirs   []string
}

func (r *fakeRunner) RunGit(ctx context.Context, workDir string, args ...string) (string, error) {
	key := strings.Join(args, " ")
	r.gitCalls = append(r.gitCalls, key)
	r.gitDirs = append(r.gitDirs, workDir)
	if r.gitFn != nil {
		return r.gitFn(ctx, workDir, args...)
	}
	return r.gitOut[key], r.gitErr[key]
}

func (r *fakeRunner) RunGH(ctx context.Context, workDir string, args ...string) (string, error) {
	key := strings.Join(args, " ")
	r.ghCalls = append(r.ghCalls, key)
	r.ghDirs = append(r.ghDirs, workDir)
	if r.ghFn != nil {
		return r.ghFn(ctx, workDir, args...)
	}
	return r.ghOut[key], r.ghErr[key]
}

type fakeSink struct {
	prURL    string
	issueURL string
}

func (s *fakeSink) RecordPullRequestURL(_ context.Context, url string) error {
	s.prURL = url
	return nil
}

func (s *fakeSink) RecordIssueURL(_ context.Context, url string) error {
	s.issueURL = url
	return nil
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
