package git

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

const commandTimeout = 60 * time.Second

// ArtifactSink records GitHub artifacts created by the tools.
type ArtifactSink interface {
	RecordPullRequestURL(ctx context.Context, url string) error
	RecordIssueURL(ctx context.Context, url string) error
}

// CommandRunner runs git and gh commands. Tests and hosts may replace it.
type CommandRunner interface {
	RunGit(ctx context.Context, workDir string, args ...string) (string, error)
	RunGH(ctx context.Context, workDir string, args ...string) (string, error)
}

type execCommandRunner struct{}

func (execCommandRunner) RunGit(ctx context.Context, workDir string, args ...string) (string, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "git", args...)
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (execCommandRunner) RunGH(ctx context.Context, workDir string, args ...string) (string, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "gh", args...)
	cmd.Dir = workDir
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return stdout.String() + stderr.String(), err
	}
	return stdout.String(), nil
}

// CreatePullRequestTool creates a GitHub pull request from the current branch.
type CreatePullRequestTool struct {
	Runner CommandRunner
	Sink   ArtifactSink
}

func NewCreatePullRequestTool(runner CommandRunner, sink ArtifactSink) *CreatePullRequestTool {
	return &CreatePullRequestTool{Runner: runner, Sink: sink}
}

func (t *CreatePullRequestTool) Name() string { return "create_pull_request" }

func (t *CreatePullRequestTool) Description() string {
	return "Create a GitHub pull request from the current branch. Commits any unstaged changes, pushes the branch, and opens a PR. Returns the PR URL."
}

func (t *CreatePullRequestTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"title": {
				"type": "string",
				"description": "PR title. If empty, inferred from commits."
			},
			"body": {
				"type": "string",
				"description": "PR body/description in markdown. If empty, auto-filled from commits."
			},
			"base_branch": {
				"type": "string",
				"description": "Target branch to merge into (e.g. main). If empty, uses repo default."
			},
			"repo_path": {
				"type": "string",
				"description": "Workspace-relative path to the git repository, for example repos/<alias>. Defaults to the workspace root for callers that still use a single root repository."
			},
			"draft": {
				"type": "boolean",
				"description": "Create as draft PR. Defaults to false."
			}
		}
	}`)
}

func (t *CreatePullRequestTool) IsReadOnly() bool      { return false }
func (t *CreatePullRequestTool) WritesGitRemote() bool { return true }
func (t *CreatePullRequestTool) IsEnabled(ctx *agentsdk.RunContext) bool {
	return agentsdk.MutatingToolEnabled(ctx, t.Name())
}
func (t *CreatePullRequestTool) NeedsApproval() bool { return false }
func (t *CreatePullRequestTool) TimeoutSeconds() int { return 0 }

type createPullRequestInput struct {
	Title      string `json:"title"`
	Body       string `json:"body"`
	BaseBranch string `json:"base_branch"`
	RepoPath   string `json:"repo_path"`
	Draft      bool   `json:"draft"`
}

type createPullRequestOutput struct {
	PRURL  string `json:"pr_url"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

func (t *CreatePullRequestTool) Execute(ctx context.Context, input json.RawMessage, workDir string) (agentsdk.ToolResult, error) {
	var in createPullRequestInput
	if err := json.Unmarshal(input, &in); err != nil {
		return prError("Invalid input: " + err.Error())
	}
	repoDir, err := resolveRepositoryWorkDir(workDir, in.RepoPath)
	if err != nil {
		return prError("repo_path rejected: " + err.Error())
	}

	runner := t.runner()
	if err := GuardBranchForPush(ctx, runner, repoDir, in.BaseBranch); err != nil {
		return prError(err.Error())
	}
	statusOut, err := runner.RunGit(ctx, repoDir, "status", "--porcelain")
	if err == nil && strings.TrimSpace(statusOut) != "" {
		if _, err := runner.RunGit(ctx, repoDir, "add", "-A"); err != nil {
			return prError("git add failed: " + err.Error())
		}
		msg := in.Title
		if msg == "" {
			msg = "changes from agent run"
		}
		if _, err := runner.RunGit(ctx, repoDir, "commit", "--no-verify", "-m", msg); err != nil {
			return prError("git commit failed: " + err.Error())
		}
	}

	pushOut, err := runner.RunGit(ctx, repoDir, "push", "--no-verify", "-u", "origin", "HEAD")
	if err != nil {
		return prError(fmt.Sprintf("git push failed: %s\n%s", err, pushOut))
	}

	args := []string{"pr", "create"}
	// Pass --head explicitly so gh does not have to infer the branch from
	// upstream state ("you must first push the current branch" failures).
	if branchOut, branchErr := runner.RunGit(ctx, repoDir, "rev-parse", "--abbrev-ref", "HEAD"); branchErr == nil {
		if branch := strings.TrimSpace(branchOut); branch != "" && branch != "HEAD" {
			args = append(args, "--head", branch)
		}
	}
	if in.Title != "" {
		args = append(args, "--title", in.Title)
	}
	if in.Body != "" {
		args = append(args, "--body", in.Body)
	}
	if in.Title == "" && in.Body == "" {
		args = append(args, "--fill")
	} else if in.Title != "" && in.Body == "" {
		args = append(args, "--body", "")
	}
	if in.BaseBranch != "" {
		args = append(args, "--base", in.BaseBranch)
	}
	if in.Draft {
		args = append(args, "--draft")
	}

	prOut, err := runner.RunGH(ctx, repoDir, args...)
	if err != nil {
		if prURL, fetchErr := ghPRViewURL(ctx, runner, repoDir); fetchErr == nil && prURL != "" {
			t.recordPR(ctx, prURL)
			return prSuccess(prURL, "PR already exists")
		}
		return prError(fmt.Sprintf("gh pr create failed: %s\n%s", err, prOut))
	}

	prURL, fetchErr := ghPRViewURL(ctx, runner, repoDir)
	if fetchErr != nil || prURL == "" {
		prURL = strings.TrimSpace(prOut)
	}
	if prURL == "" {
		return prError("gh pr create returned empty output")
	}

	t.recordPR(ctx, prURL)
	return prSuccess(prURL, "PR created successfully")
}

func (t *CreatePullRequestTool) runner() CommandRunner {
	if t.Runner != nil {
		return t.Runner
	}
	return execCommandRunner{}
}

func (t *CreatePullRequestTool) recordPR(ctx context.Context, url string) {
	if t.Sink == nil {
		return
	}
	if err := t.Sink.RecordPullRequestURL(ctx, url); err != nil {
		log.Printf("WARN: failed to record pull request URL: %v", err)
	}
}

// GuardBranchForPush refuses to push from a branch that must never receive
// direct pushes: the repository default branch (resolved from
// refs/remotes/origin/HEAD when available), main, master, a detached HEAD, or
// a branch equal to the requested PR base. Hosts can reuse it to guard their
// own push paths. A nil error means the current branch is a pushable work
// branch.
func GuardBranchForPush(ctx context.Context, runner CommandRunner, repoDir, baseBranch string) error {
	out, err := runner.RunGit(ctx, repoDir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return fmt.Errorf("refusing to push: could not determine the current branch: %v\n%s", err, out)
	}
	branch := strings.TrimSpace(out)
	if branch == "" {
		return fmt.Errorf("refusing to push: could not determine the current branch")
	}
	if branch == "HEAD" {
		return fmt.Errorf("refusing to push from a detached HEAD; create a work branch first (git checkout -b <branch>)")
	}
	if branch == "main" || branch == "master" {
		return fmt.Errorf("refusing to push protected branch %q; create a work branch first (git checkout -b <branch>) and open the pull request from it", branch)
	}
	if def := remoteDefaultBranch(ctx, runner, repoDir); def != "" && branch == def {
		return fmt.Errorf("refusing to push default branch %q; create a work branch first (git checkout -b <branch>) and open the pull request from it", branch)
	}
	if base := strings.TrimSpace(baseBranch); base != "" && base == branch {
		return fmt.Errorf("current branch %q is the same as base_branch; create a work branch first (git checkout -b <branch>)", branch)
	}
	return nil
}

// remoteDefaultBranch resolves origin's default branch (e.g. "main") from
// refs/remotes/origin/HEAD. Returns "" when it cannot be determined, e.g. in
// clones where origin/HEAD was never set.
func remoteDefaultBranch(ctx context.Context, runner CommandRunner, repoDir string) string {
	out, err := runner.RunGit(ctx, repoDir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(out), "origin/")
}

func ghPRViewURL(ctx context.Context, runner CommandRunner, workDir string) (string, error) {
	out, err := runner.RunGH(ctx, workDir, "pr", "view", "--json", "url", "-q", ".url")
	if err != nil {
		return "", err
	}
	url := strings.TrimSpace(out)
	if !strings.HasPrefix(url, "https://") {
		return "", fmt.Errorf("unexpected pr view output: %s", url)
	}
	return url, nil
}

func prSuccess(prURL, status string) (agentsdk.ToolResult, error) {
	out := createPullRequestOutput{PRURL: prURL, Status: status}
	b, _ := json.Marshal(out)
	return agentsdk.ToolResult{Content: string(b)}, nil
}

func prError(msg string) (agentsdk.ToolResult, error) {
	out := createPullRequestOutput{Status: "error", Error: msg}
	b, _ := json.Marshal(out)
	return agentsdk.ToolResult{Content: string(b), IsError: true}, nil
}

// CreateIssueTool creates a GitHub issue in the current repository.
type CreateIssueTool struct {
	Runner CommandRunner
	Sink   ArtifactSink
}

func NewCreateIssueTool(runner CommandRunner, sink ArtifactSink) *CreateIssueTool {
	return &CreateIssueTool{Runner: runner, Sink: sink}
}

func (t *CreateIssueTool) Name() string { return "create_github_issue" }

func (t *CreateIssueTool) Description() string {
	return "Create a GitHub issue in the current repository. Use this to file bug reports, feature requests, improvement recommendations, or analysis findings. Returns the issue URL."
}

func (t *CreateIssueTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"title": {
				"type": "string",
				"description": "Issue title. Required."
			},
			"body": {
				"type": "string",
				"description": "Issue body in markdown. Should contain detailed description, analysis, and any recommendations."
			},
			"labels": {
				"type": "array",
				"items": { "type": "string" },
				"description": "Labels to apply to the issue (e.g. [\"meta-harness\", \"enhancement\"])."
			},
			"assignees": {
				"type": "array",
				"items": { "type": "string" },
				"description": "GitHub usernames to assign to the issue."
			},
			"repo_path": {
				"type": "string",
				"description": "Workspace-relative path to the git repository, for example repos/<alias>. Defaults to the workspace root for callers that still use a single root repository."
			}
		},
		"required": ["title"]
	}`)
}

func (t *CreateIssueTool) IsReadOnly() bool { return false }
func (t *CreateIssueTool) IsEnabled(ctx *agentsdk.RunContext) bool {
	return agentsdk.MutatingToolEnabled(ctx, t.Name())
}
func (t *CreateIssueTool) NeedsApproval() bool { return false }
func (t *CreateIssueTool) TimeoutSeconds() int { return 0 }

type createIssueInput struct {
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	Labels    []string `json:"labels"`
	Assignees []string `json:"assignees"`
	RepoPath  string   `json:"repo_path"`
}

type createIssueOutput struct {
	IssueURL string `json:"issue_url"`
	Status   string `json:"status"`
	Error    string `json:"error,omitempty"`
}

func (t *CreateIssueTool) Execute(ctx context.Context, input json.RawMessage, workDir string) (agentsdk.ToolResult, error) {
	var in createIssueInput
	if err := json.Unmarshal(input, &in); err != nil {
		return issueError("Invalid input: " + err.Error())
	}
	if strings.TrimSpace(in.Title) == "" {
		return issueError("title is required")
	}
	repoDir, err := resolveRepositoryWorkDir(workDir, in.RepoPath)
	if err != nil {
		return issueError("repo_path rejected: " + err.Error())
	}

	args := []string{"issue", "create", "--title", in.Title}
	if in.Body != "" {
		args = append(args, "--body", in.Body)
	}
	for _, label := range in.Labels {
		args = append(args, "--label", label)
	}
	for _, assignee := range in.Assignees {
		args = append(args, "--assignee", assignee)
	}

	out, err := t.runner().RunGH(ctx, repoDir, args...)
	if err != nil {
		// Nonexistent labels abort issue creation entirely; retry without
		// labels rather than losing the report over metadata.
		if len(in.Labels) > 0 && strings.Contains(out, "could not add label") {
			retryArgs := []string{"issue", "create", "--title", in.Title}
			if in.Body != "" {
				retryArgs = append(retryArgs, "--body", in.Body)
			}
			for _, assignee := range in.Assignees {
				retryArgs = append(retryArgs, "--assignee", assignee)
			}
			retryOut, retryErr := t.runner().RunGH(ctx, repoDir, retryArgs...)
			if retryErr == nil {
				if issueURL := strings.TrimSpace(retryOut); strings.HasPrefix(issueURL, "https://") {
					t.recordIssue(ctx, issueURL)
					return issueSuccessWithStatus(issueURL, fmt.Sprintf(
						"Issue created without labels %v (labels do not exist in this repository; create them first or omit them).", in.Labels))
				}
			}
		}
		return issueError(fmt.Sprintf("gh issue create failed: %s\n%s", err, out))
	}

	issueURL := strings.TrimSpace(out)
	if issueURL == "" {
		return issueError("gh issue create returned empty output")
	}
	if !strings.HasPrefix(issueURL, "https://") {
		return issueError("gh issue create returned unexpected output: " + issueURL)
	}
	t.recordIssue(ctx, issueURL)
	return issueSuccess(issueURL)
}

func (t *CreateIssueTool) runner() CommandRunner {
	if t.Runner != nil {
		return t.Runner
	}
	return execCommandRunner{}
}

func (t *CreateIssueTool) recordIssue(ctx context.Context, url string) {
	if t.Sink == nil {
		return
	}
	if err := t.Sink.RecordIssueURL(ctx, url); err != nil {
		log.Printf("WARN: failed to record issue URL: %v", err)
	}
}

func issueSuccess(issueURL string) (agentsdk.ToolResult, error) {
	return issueSuccessWithStatus(issueURL, "Issue created successfully")
}

func issueSuccessWithStatus(issueURL, status string) (agentsdk.ToolResult, error) {
	out := createIssueOutput{IssueURL: issueURL, Status: status}
	b, _ := json.Marshal(out)
	return agentsdk.ToolResult{Content: string(b)}, nil
}

func issueError(msg string) (agentsdk.ToolResult, error) {
	out := createIssueOutput{Status: "error", Error: msg}
	b, _ := json.Marshal(out)
	return agentsdk.ToolResult{Content: string(b), IsError: true}, nil
}
