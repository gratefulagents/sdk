package git

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAttachRepositoryClonesIntoWorkspaceRepoList(t *testing.T) {
	workDir := t.TempDir()
	runner := &fakeRunner{gitFn: cloneFakeRepo}
	tool := NewAttachRepositoryTool(
		runner,
		WithAttachRepositoryDefaultBaseBranch("main"),
		WithAttachRepositoryDefaultBranchName("agent-run-123"),
	)

	result, err := tool.Execute(context.Background(), json.RawMessage(`{"repository":"acme/helm-charts"}`), workDir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("Execute() returned error result: %s", result.Content)
	}
	if strings.Contains(result.Content, `"primary"`) {
		t.Fatalf("result still contains primary field: %s", result.Content)
	}
	if !strings.Contains(result.Content, `"path":"repos/helm-charts"`) {
		t.Fatalf("result = %s", result.Content)
	}
	repoPath := filepath.Join(workDir, "repos", "helm-charts")
	if _, err := os.Stat(filepath.Join(repoPath, ".git")); err != nil {
		t.Fatalf("repo .git missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoPath, "README.md")); err != nil {
		t.Fatalf("repo file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".git")); err == nil {
		t.Fatalf("workspace root unexpectedly became a git repo")
	}

	wantClonePrefix := "clone --branch main https://github.com/acme/helm-charts.git"
	if !strings.HasPrefix(runner.gitCalls[0], wantClonePrefix) || runner.gitCalls[1] != "checkout -B agent-run-123" {
		t.Fatalf("git calls = %#v, want clone prefix %q then checkout", runner.gitCalls, wantClonePrefix)
	}
	resolvedWorkDir, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		t.Fatal(err)
	}
	if runner.gitDirs[0] != resolvedWorkDir {
		t.Fatalf("clone cwd = %q, want workspace root %q", runner.gitDirs[0], resolvedWorkDir)
	}
}

func TestAttachRepositoryClonesMultipleWorkspaceRepos(t *testing.T) {
	workDir := t.TempDir()
	runner := &fakeRunner{gitFn: cloneFakeRepo}
	tool := NewAttachRepositoryTool(runner)

	for _, input := range []string{
		`{"repository":"https://github.com/acme/service","alias":"payments"}`,
		`{"repository":"acme/helm-values","alias":"values"}`,
	} {
		result, err := tool.Execute(context.Background(), json.RawMessage(input), workDir)
		if err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
		if result.IsError {
			t.Fatalf("Execute() returned error result: %s", result.Content)
		}
	}
	for _, alias := range []string{"payments", "values"} {
		if _, err := os.Stat(filepath.Join(workDir, "repos", alias, ".git")); err != nil {
			t.Fatalf("repo %s was not cloned: %v", alias, err)
		}
	}
}

func TestAttachRepositoryCreatesMissingWorkspace(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	runner := &fakeRunner{gitFn: cloneFakeRepo}

	result, err := NewAttachRepositoryTool(runner).Execute(context.Background(), json.RawMessage(`{"repository":"acme/repo"}`), workDir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("Execute() returned error result: %s", result.Content)
	}
	if _, err := os.Stat(filepath.Join(workDir, "repos", "repo", ".git")); err != nil {
		t.Fatalf("repo .git missing: %v", err)
	}
}

func TestAttachRepositoryReusesDuplicateAliasForSameRepository(t *testing.T) {
	workDir := t.TempDir()
	runner := &fakeRunner{gitFn: cloneFakeRepo}
	tool := NewAttachRepositoryTool(runner)

	if result, err := tool.Execute(context.Background(), json.RawMessage(`{"repository":"acme/repo","alias":"same"}`), workDir); err != nil || result.IsError {
		t.Fatalf("first Execute() result=%+v err=%v", result, err)
	}
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"repository":"acme/repo","alias":"same"}`), workDir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("Execute() returned error result: %s", result.Content)
	}
	if !strings.Contains(result.Content, `"status":"already_attached"`) || !strings.Contains(result.Content, `"path":"repos/same"`) {
		t.Fatalf("result = %s, want already_attached for repos/same", result.Content)
	}
	cloneCalls := 0
	for _, call := range runner.gitCalls {
		if strings.HasPrefix(call, "clone ") {
			cloneCalls++
		}
	}
	if cloneCalls != 1 {
		t.Fatalf("clone calls = %d (%#v), want exactly one clone", cloneCalls, runner.gitCalls)
	}
}

func TestAttachRepositoryRejectsDuplicateAliasWithDifferentRepository(t *testing.T) {
	workDir := t.TempDir()
	runner := &fakeRunner{gitFn: cloneFakeRepo}
	tool := NewAttachRepositoryTool(runner)

	if result, err := tool.Execute(context.Background(), json.RawMessage(`{"repository":"acme/repo","alias":"same"}`), workDir); err != nil || result.IsError {
		t.Fatalf("first Execute() result=%+v err=%v", result, err)
	}
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"repository":"acme/other","alias":"same"}`), workDir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content, "already exists") || !strings.Contains(result.Content, "with origin") {
		t.Fatalf("result = %+v, want duplicate alias error", result)
	}
}

func TestAttachRepositoryRejectsDuplicateAliasThatIsNotGitRepository(t *testing.T) {
	workDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workDir, "repos", "same"), 0o755); err != nil {
		t.Fatal(err)
	}

	result, err := NewAttachRepositoryTool(&fakeRunner{}).Execute(context.Background(), json.RawMessage(`{"repository":"acme/repo","alias":"same"}`), workDir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content, "not a git repository") {
		t.Fatalf("result = %+v, want non-git duplicate alias error", result)
	}
}

func TestSameRepositoryURLMatchesCommonGitHubForms(t *testing.T) {
	tests := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{
			name: "https and ssh",
			a:    "https://github.com/acme/repo.git",
			b:    "git@github.com:acme/repo.git",
			want: true,
		},
		{
			name: "https and ssh url",
			a:    "https://github.com/acme/repo",
			b:    "ssh://git@github.com/acme/repo.git",
			want: true,
		},
		{
			name: "different repo",
			a:    "https://github.com/acme/repo.git",
			b:    "https://github.com/acme/other.git",
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sameRepositoryURL(tt.a, tt.b); got != tt.want {
				t.Fatalf("sameRepositoryURL(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestAttachRepositoryExcludesRepoListFromWorkspaceRootGit(t *testing.T) {
	workDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{gitFn: cloneFakeRepo}

	result, err := NewAttachRepositoryTool(runner).Execute(context.Background(), json.RawMessage(`{"repository":"acme/repo"}`), workDir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("Execute() returned error result: %s", result.Content)
	}
	exclude, err := os.ReadFile(filepath.Join(workDir, ".git", "info", "exclude"))
	if err != nil {
		t.Fatalf("exclude missing: %v", err)
	}
	if !strings.Contains(string(exclude), "repos/") {
		t.Fatalf("exclude = %q, want repos/", exclude)
	}
}

func cloneFakeRepo(_ context.Context, workDir string, args ...string) (string, error) {
	if len(args) == 0 {
		return "", nil
	}
	switch args[0] {
	case "clone":
		repoURL := args[len(args)-2]
		dest := args[len(args)-1]
		if err := os.MkdirAll(filepath.Join(dest, ".git", "info"), 0o755); err != nil {
			return "", err
		}
		config := "[remote \"origin\"]\n\turl = " + repoURL + "\n"
		if err := os.WriteFile(filepath.Join(dest, ".git", "config"), []byte(config), 0o644); err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(dest, "README.md"), []byte("hello\n"), 0o644); err != nil {
			return "", err
		}
	case "remote":
		if len(args) == 3 && args[1] == "get-url" && args[2] == "origin" {
			config, err := os.ReadFile(filepath.Join(workDir, ".git", "config"))
			if err != nil {
				return "", err
			}
			for _, line := range strings.Split(string(config), "\n") {
				if url, ok := strings.CutPrefix(strings.TrimSpace(line), "url = "); ok {
					return url + "\n", nil
				}
			}
			return "", fmt.Errorf("origin remote not found")
		}
	case "checkout":
	}
	return "", nil
}

func requireRepoPathTestRepo(t *testing.T, workDir, repoPath string) string {
	t.Helper()
	repoDir := filepath.Join(workDir, filepath.FromSlash(repoPath))
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestCreatePullRequestToolUsesRepoPath(t *testing.T) {
	workDir := t.TempDir()
	repoDir := requireRepoPathTestRepo(t, workDir, "repos/helm")
	runner := &fakeRunner{
		gitOut: map[string]string{
			"status --porcelain": "",
		},
		ghOut: map[string]string{
			"pr create --fill":           "https://github.com/acme/helm/pull/9\n",
			"pr view --json url -q .url": "https://github.com/acme/helm/pull/9\n",
		},
	}

	result, err := NewCreatePullRequestTool(runner, nil).Execute(context.Background(), json.RawMessage(`{"repo_path":"repos/helm"}`), workDir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("Execute() returned error result: %s", result.Content)
	}
	if !reflect.DeepEqual(runner.gitDirs, []string{repoDir, repoDir}) {
		t.Fatalf("git dirs = %#v, want %#v", runner.gitDirs, []string{repoDir, repoDir})
	}
	if !reflect.DeepEqual(runner.ghDirs, []string{repoDir, repoDir}) {
		t.Fatalf("gh dirs = %#v, want %#v", runner.ghDirs, []string{repoDir, repoDir})
	}
}

func TestCreatePullRequestToolRejectsRepoPathOutsideWorkspace(t *testing.T) {
	workDir := t.TempDir()
	result, err := NewCreatePullRequestTool(&fakeRunner{}, nil).Execute(context.Background(), json.RawMessage(`{"repo_path":"../outside"}`), workDir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content, "repo_path rejected") {
		t.Fatalf("result = %+v, want repo_path rejection", result)
	}
}

func TestCreateIssueToolUsesRepoPath(t *testing.T) {
	workDir := t.TempDir()
	repoDir := requireRepoPathTestRepo(t, workDir, "repos/helm")
	runner := &fakeRunner{
		ghOut: map[string]string{
			"issue create --title Bug": "https://github.com/acme/helm/issues/3\n",
		},
	}

	result, err := NewCreateIssueTool(runner, nil).Execute(context.Background(), json.RawMessage(`{"title":"Bug","repo_path":"repos/helm"}`), workDir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("Execute() returned error result: %s", result.Content)
	}
	if !reflect.DeepEqual(runner.ghDirs, []string{repoDir}) {
		t.Fatalf("gh dirs = %#v, want %#v", runner.ghDirs, []string{repoDir})
	}
}
