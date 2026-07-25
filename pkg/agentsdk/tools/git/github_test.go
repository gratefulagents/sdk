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
			"rev-parse --abbrev-ref HEAD": "agent/work\n",
			"status --porcelain":          " M changed.go\n?? new.go\n",
		},
		ghOut: map[string]string{
			"pr create --head agent/work --title Add feature --body  --base main --draft": "",
			"pr view --json url -q .url": "https://github.com/acme/repo/pull/7\n",
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
	wantGit := []string{
		"rev-parse --abbrev-ref HEAD",
		"symbolic-ref --short refs/remotes/origin/HEAD",
		"status --porcelain",
		"add -A",
		"commit --no-verify -m Add feature",
		"push --no-verify -u origin HEAD",
		"rev-parse --abbrev-ref HEAD",
	}
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
			"rev-parse --abbrev-ref HEAD": "agent/work\n",
			"status --porcelain":          "?? new.go\n",
		},
		ghOut: map[string]string{
			"pr create --head agent/work --fill": "https://github.com/acme/repo/pull/9\n",
			"pr view --json url -q .url":         "https://github.com/acme/repo/pull/9\n",
		},
	}

	result, err := NewCreatePullRequestTool(runner, nil).Execute(context.Background(), json.RawMessage(`{}`), "/repo")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("Execute() returned error result: %s", result.Content)
	}
	wantGit := []string{
		"rev-parse --abbrev-ref HEAD",
		"symbolic-ref --short refs/remotes/origin/HEAD",
		"status --porcelain",
		"add -A",
		"commit --no-verify -m changes from agent run",
		"push --no-verify -u origin HEAD",
		"rev-parse --abbrev-ref HEAD",
	}
	if !reflect.DeepEqual(runner.gitCalls, wantGit) {
		t.Fatalf("git calls = %#v, want %#v", runner.gitCalls, wantGit)
	}
}

func TestCreatePullRequestToolUsesExistingPRWhenCreateFails(t *testing.T) {
	runner := &fakeRunner{
		gitOut: map[string]string{
			"rev-parse --abbrev-ref HEAD": "agent/work\n",
		},
		ghOut: map[string]string{
			"pr view --json url -q .url": "https://github.com/acme/repo/pull/8\n",
		},
		ghErr: map[string]error{
			"pr create --head agent/work --fill": errors.New("already exists"),
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

func TestCreatePullRequestToolRefusesProtectedAndUnknownBranches(t *testing.T) {
	cases := []struct {
		name    string
		gitOut  map[string]string
		gitErr  map[string]error
		input   map[string]any
		wantMsg string
	}{
		{
			name:    "main is refused",
			gitOut:  map[string]string{"rev-parse --abbrev-ref HEAD": "main\n"},
			wantMsg: "refusing to push protected branch",
		},
		{
			name:    "master is refused",
			gitOut:  map[string]string{"rev-parse --abbrev-ref HEAD": "master\n"},
			wantMsg: "refusing to push protected branch",
		},
		{
			name: "remote default branch is refused",
			gitOut: map[string]string{
				"rev-parse --abbrev-ref HEAD":                   "develop\n",
				"symbolic-ref --short refs/remotes/origin/HEAD": "origin/develop\n",
			},
			wantMsg: "refusing to push default branch",
		},
		{
			name:    "detached HEAD is refused",
			gitOut:  map[string]string{"rev-parse --abbrev-ref HEAD": "HEAD\n"},
			wantMsg: "detached HEAD",
		},
		{
			name:    "branch equal to base_branch is refused",
			gitOut:  map[string]string{"rev-parse --abbrev-ref HEAD": "release\n"},
			input:   map[string]any{"base_branch": "release"},
			wantMsg: "is the same as base_branch",
		},
		{
			name:    "undeterminable branch is refused",
			gitErr:  map[string]error{"rev-parse --abbrev-ref HEAD": errors.New("not a git repository")},
			wantMsg: "could not determine the current branch",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{gitOut: tc.gitOut, gitErr: tc.gitErr}
			input := tc.input
			if input == nil {
				input = map[string]any{}
			}
			result, err := NewCreatePullRequestTool(runner, nil).Execute(context.Background(), mustJSON(t, input), "/repo")
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			if !result.IsError {
				t.Fatalf("Execute() = %s, want refusal", result.Content)
			}
			if !strings.Contains(result.Content, tc.wantMsg) {
				t.Fatalf("result = %s, want message containing %q", result.Content, tc.wantMsg)
			}
			for _, call := range runner.gitCalls {
				if strings.HasPrefix(call, "push") {
					t.Fatalf("push was executed despite refusal: %#v", runner.gitCalls)
				}
			}
			if len(runner.ghCalls) != 0 {
				t.Fatalf("gh was invoked despite refusal: %#v", runner.ghCalls)
			}
		})
	}
}

func TestCreateIssueToolRunsGitHubFlowAndRecordsURL(t *testing.T) {
	runner := &fakeRunner{
		ghOut: map[string]string{
			"label list --limit 1000 --json name":                                             `[{"name":"bug"},{"name":"sdk"}]`,
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

func TestCreateIssueToolCreatesMissingLabels(t *testing.T) {
	runner := &fakeRunner{
		ghOut: map[string]string{
			"label list --limit 1000 --json name":                                      `[]`,
			"issue create --title Bug report --body details --label tests --label sdk": "https://github.com/acme/repo/issues/12\n",
		},
		ghErr: map[string]error{
			"label create sdk --color BFD4F2": errors.New("exit status 1"),
		},
	}
	runner.ghOut["label create sdk --color BFD4F2"] = "label already exists\n"

	result, err := NewCreateIssueTool(runner, nil).Execute(context.Background(), mustJSON(t, map[string]any{
		"title":  "Bug report",
		"body":   "details",
		"labels": []string{" tests ", "sdk", "TESTS"},
	}), "/repo")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("Execute() returned error result: %s", result.Content)
	}
	wantCalls := []string{
		"label list --limit 1000 --json name",
		"label create tests --color BFD4F2",
		"label create sdk --color BFD4F2",
		"issue create --title Bug report --body details --label tests --label sdk",
	}
	if !reflect.DeepEqual(runner.ghCalls, wantCalls) {
		t.Fatalf("gh calls = %#v, want %#v", runner.ghCalls, wantCalls)
	}
}

func TestCreateIssueToolSkipsExistingLabels(t *testing.T) {
	runner := &fakeRunner{
		ghOut: map[string]string{
			"label list --limit 1000 --json name":                     `[{"name":"Bug"},{"name":"sdk"}]`,
			"issue create --title Bug report --label bug --label SDK": "https://github.com/acme/repo/issues/12\n",
		},
	}

	result, err := NewCreateIssueTool(runner, nil).Execute(context.Background(), mustJSON(t, map[string]any{
		"title":  "Bug report",
		"labels": []string{" bug ", "SDK", "BUG", " "},
	}), "/repo")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("Execute() returned error result: %s", result.Content)
	}
	wantCalls := []string{
		"label list --limit 1000 --json name",
		"issue create --title Bug report --label bug --label SDK",
	}
	if !reflect.DeepEqual(runner.ghCalls, wantCalls) {
		t.Fatalf("gh calls = %#v, want %#v", runner.ghCalls, wantCalls)
	}
}

func TestCreateIssueToolStopsWhenLabelListingFails(t *testing.T) {
	runner := &fakeRunner{
		ghOut: map[string]string{
			"label list --limit 1000 --json name": "permission denied\n",
		},
		ghErr: map[string]error{
			"label list --limit 1000 --json name": errors.New("exit status 1"),
		},
	}

	result, err := NewCreateIssueTool(runner, nil).Execute(context.Background(), mustJSON(t, map[string]any{
		"title":  "Bug report",
		"labels": []string{"tests"},
	}), "/repo")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content, "gh label list failed") {
		t.Fatalf("result = %+v, want label listing failure", result)
	}
	if len(runner.ghCalls) != 1 {
		t.Fatalf("gh calls = %#v, want only label listing", runner.ghCalls)
	}
}

func TestCreateIssueToolStopsWhenLabelCreationFails(t *testing.T) {
	runner := &fakeRunner{
		ghOut: map[string]string{
			"label list --limit 1000 --json name": `[]`,
			"label create tests --color BFD4F2":   "permission denied\n",
		},
		ghErr: map[string]error{
			"label create tests --color BFD4F2": errors.New("exit status 1"),
		},
	}

	result, err := NewCreateIssueTool(runner, nil).Execute(context.Background(), mustJSON(t, map[string]any{
		"title":  "Bug report",
		"labels": []string{"tests"},
	}), "/repo")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content, "gh label create failed") {
		t.Fatalf("result = %+v, want label creation failure", result)
	}
	for _, call := range runner.ghCalls {
		if strings.HasPrefix(call, "issue create") {
			t.Fatalf("issue was created after label creation failed: %#v", runner.ghCalls)
		}
	}
}

func TestCreateIssueToolDoesNotDropLabelsWhenIssueCreationFails(t *testing.T) {
	runner := &fakeRunner{
		ghOut: map[string]string{
			"label list --limit 1000 --json name":                          `[{"name":"tests"}]`,
			"issue create --title Bug report --body details --label tests": "could not add label: 'tests' not found\n",
		},
		ghErr: map[string]error{
			"issue create --title Bug report --body details --label tests": errors.New("exit status 1"),
		},
	}

	result, err := NewCreateIssueTool(runner, nil).Execute(context.Background(), mustJSON(t, map[string]any{
		"title":  "Bug report",
		"body":   "details",
		"labels": []string{"tests"},
	}), "/repo")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content, "gh issue create failed") {
		t.Fatalf("result = %+v, want issue creation failure", result)
	}
	wantCalls := []string{
		"label list --limit 1000 --json name",
		"issue create --title Bug report --body details --label tests",
	}
	if !reflect.DeepEqual(runner.ghCalls, wantCalls) {
		t.Fatalf("gh calls = %#v, want %#v", runner.ghCalls, wantCalls)
	}
}

func TestCreateIssueToolDoesNotRetryOtherFailures(t *testing.T) {
	runner := &fakeRunner{
		ghOut: map[string]string{
			"issue create --title Bug report": "GraphQL: rate limited\n",
		},
		ghErr: map[string]error{
			"issue create --title Bug report": errors.New("exit status 1"),
		},
	}

	result, err := NewCreateIssueTool(runner, nil).Execute(context.Background(), mustJSON(t, map[string]any{"title": "Bug report"}), "/repo")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content, "gh issue create failed") {
		t.Fatalf("result = %+v, want create failure", result)
	}
	if len(runner.ghCalls) != 1 {
		t.Fatalf("gh calls = %v, want single attempt", runner.ghCalls)
	}
}
