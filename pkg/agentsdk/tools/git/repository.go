package git

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	"github.com/gratefulagents/sdk/pkg/agentsdk/tools/internal/pathutil"
)

const defaultAttachedRepoDir = "repos"

// AttachRepositoryTool clones git repositories into the SDK workspace.
//
// Repositories are cloned under repos/<alias> by default, keeping every clone
// inside the existing workspace boundary so file, search, shell, and GitHub
// tools can address them by workspace-relative paths.
type AttachRepositoryTool struct {
	Runner            CommandRunner
	DefaultBaseBranch string
	DefaultBranchName string
	RepoStoreDir      string
}

// AttachRepositoryOption configures AttachRepositoryTool.
type AttachRepositoryOption func(*AttachRepositoryTool)

func NewAttachRepositoryTool(runner CommandRunner, opts ...AttachRepositoryOption) *AttachRepositoryTool {
	t := &AttachRepositoryTool{Runner: runner}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// WithAttachRepositoryDefaultBaseBranch sets the branch used when input omits
// base_branch.
func WithAttachRepositoryDefaultBaseBranch(branch string) AttachRepositoryOption {
	return func(t *AttachRepositoryTool) { t.DefaultBaseBranch = strings.TrimSpace(branch) }
}

// WithAttachRepositoryDefaultBranchName sets the branch checked out after clone
// when input omits branch_name.
func WithAttachRepositoryDefaultBranchName(branch string) AttachRepositoryOption {
	return func(t *AttachRepositoryTool) { t.DefaultBranchName = strings.TrimSpace(branch) }
}

// WithAttachRepositoryStoreDir sets the workspace-relative parent directory for
// repositories. The default is repos.
func WithAttachRepositoryStoreDir(path string) AttachRepositoryOption {
	return func(t *AttachRepositoryTool) { t.RepoStoreDir = strings.TrimSpace(path) }
}

func (t *AttachRepositoryTool) Name() string { return "attach_repository" }

func (t *AttachRepositoryTool) Description() string {
	return "Clone a git repository into this SDK workspace's repository list. Repositories are cloned under repos/<alias> by default and can be used with file/search tools or passed to create_pull_request via repo_path."
}

func (t *AttachRepositoryTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"repository": {
				"type": "string",
				"description": "Repository to clone. Accepts a git URL, GitHub URL, github.com/owner/repo, or owner/repo shorthand."
			},
			"repo": {
				"type": "string",
				"description": "Alias for repository."
			},
			"base_branch": {
				"type": "string",
				"description": "Branch to clone from. If empty, uses the tool default or the repository default branch."
			},
			"branch_name": {
				"type": "string",
				"description": "Branch name to create/check out after cloning. If empty, uses the tool default or keeps the cloned branch."
			},
			"alias": {
				"type": "string",
				"description": "Directory name under the workspace repository list. Defaults to the repository name."
			}
		}
	}`)
}

func (t *AttachRepositoryTool) IsReadOnly() bool { return false }
func (t *AttachRepositoryTool) IsEnabled(ctx *agentsdk.RunContext) bool {
	return ctx == nil || ctx.ToolAccessLevel != agentsdk.ToolAccessLevelReadOnly
}
func (t *AttachRepositoryTool) NeedsApproval() bool { return false }
func (t *AttachRepositoryTool) TimeoutSeconds() int { return 0 }

type attachRepositoryInput struct {
	Repository string `json:"repository"`
	Repo       string `json:"repo"`
	BaseBranch string `json:"base_branch"`
	BranchName string `json:"branch_name"`
	Alias      string `json:"alias"`
}

type attachRepositoryOutput struct {
	Status       string `json:"status"`
	Repository   string `json:"repository,omitempty"`
	Path         string `json:"path,omitempty"`
	AbsolutePath string `json:"absolute_path,omitempty"`
	BaseBranch   string `json:"base_branch,omitempty"`
	BranchName   string `json:"branch_name,omitempty"`
	Error        string `json:"error,omitempty"`
}

func (t *AttachRepositoryTool) Execute(ctx context.Context, input json.RawMessage, workDir string) (agentsdk.ToolResult, error) {
	var in attachRepositoryInput
	if err := json.Unmarshal(input, &in); err != nil {
		return attachRepositoryError("Invalid input: " + err.Error())
	}

	repoURL, repoName, err := normalizeRepositoryInput(firstNonEmpty(in.Repository, in.Repo))
	if err != nil {
		return attachRepositoryError(err.Error())
	}

	baseBranch := firstNonEmpty(strings.TrimSpace(in.BaseBranch), t.DefaultBaseBranch)
	branchName := firstNonEmpty(strings.TrimSpace(in.BranchName), t.DefaultBranchName)
	alias := sanitizeRepositoryAlias(firstNonEmpty(strings.TrimSpace(in.Alias), repoName))
	if alias == "" {
		return attachRepositoryError("alias could not be derived from repository")
	}
	return t.attachRepository(ctx, workDir, repoURL, alias, baseBranch, branchName)
}

func (t *AttachRepositoryTool) attachRepository(ctx context.Context, workDir, repoURL, alias, baseBranch, branchName string) (agentsdk.ToolResult, error) {
	if strings.TrimSpace(workDir) == "" {
		return attachRepositoryError("workspace root is required")
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return attachRepositoryError(fmt.Sprintf("creating workspace: %v", err))
	}
	storeDir := firstNonEmpty(t.RepoStoreDir, defaultAttachedRepoDir)
	workspaceRoot, err := pathutil.ResolveWorkspace(workDir, ".")
	if err != nil {
		return attachRepositoryError(fmt.Sprintf("workspace path rejected: %v", err))
	}
	storeAbs, err := pathutil.ResolveWorkspace(workDir, storeDir)
	if err != nil {
		return attachRepositoryError(fmt.Sprintf("repository store path rejected: %v", err))
	}
	dest := filepath.Join(storeAbs, alias)
	if rel, err := filepath.Rel(storeAbs, dest); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return attachRepositoryError("repository alias resolves outside the repository store")
	}
	runner := t.runner()
	if _, err := os.Stat(dest); err == nil {
		return attachExistingRepository(ctx, runner, workspaceRoot, dest, repoURL, alias, baseBranch, branchName)
	} else if !os.IsNotExist(err) {
		return attachRepositoryError(fmt.Sprintf("checking repository destination: %v", err))
	}
	if err := os.MkdirAll(storeAbs, 0o755); err != nil {
		return attachRepositoryError(fmt.Sprintf("creating repository store: %v", err))
	}

	if out, err := cloneRepository(ctx, runner, workspaceRoot, repoURL, dest, baseBranch); err != nil {
		return attachRepositoryError(fmt.Sprintf("git clone failed: %v\n%s", err, out))
	}
	if branchName != "" {
		if out, err := runner.RunGit(ctx, dest, "checkout", "-B", branchName); err != nil {
			return attachRepositoryError(fmt.Sprintf("git checkout failed: %v\n%s", err, out))
		}
	}
	if isGitRepository(workDir) {
		excludePattern := repositoryStoreExcludePattern(workspaceRoot, storeAbs)
		if excludePattern != "" {
			if err := ensureGitInfoExclude(workDir, excludePattern); err != nil {
				return attachRepositoryError(fmt.Sprintf("updating git exclude: %v", err))
			}
		}
	}

	return attachedRepositoryResult(workspaceRoot, dest, "attached", repoURL, baseBranch, branchName)
}

func repositoryStoreExcludePattern(workspaceRoot, storeAbs string) string {
	rel, err := filepath.Rel(workspaceRoot, storeAbs)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return filepath.ToSlash(rel) + "/"
}

func (t *AttachRepositoryTool) runner() CommandRunner {
	if t.Runner != nil {
		return t.Runner
	}
	return execCommandRunner{}
}

func cloneRepository(ctx context.Context, runner CommandRunner, workDir, repoURL, dest, baseBranch string) (string, error) {
	args := []string{"clone"}
	if strings.TrimSpace(baseBranch) != "" {
		args = append(args, "--branch", strings.TrimSpace(baseBranch))
	}
	args = append(args, repoURL, dest)
	return runner.RunGit(ctx, workDir, args...)
}

func attachExistingRepository(ctx context.Context, runner CommandRunner, workspaceRoot, dest, repoURL, alias, baseBranch, branchName string) (agentsdk.ToolResult, error) {
	if !isGitRepository(dest) {
		return attachRepositoryError(fmt.Sprintf("repository alias %q already exists at %s but is not a git repository", alias, dest))
	}

	origin, err := runner.RunGit(ctx, dest, "remote", "get-url", "origin")
	if err != nil {
		return attachRepositoryError(fmt.Sprintf("repository alias %q already exists at %s but its origin could not be read: %v\n%s", alias, dest, err, origin))
	}
	origin = strings.TrimSpace(origin)
	if !sameRepositoryURL(origin, repoURL) {
		return attachRepositoryError(fmt.Sprintf("repository alias %q already exists at %s with origin %q, not %q", alias, dest, origin, repoURL))
	}

	return attachedRepositoryResult(workspaceRoot, dest, "already_attached", repoURL, baseBranch, branchName)
}

func attachedRepositoryResult(workspaceRoot, dest, status, repoURL, baseBranch, branchName string) (agentsdk.ToolResult, error) {
	relPath, err := filepath.Rel(workspaceRoot, dest)
	if err != nil {
		return attachRepositoryError(fmt.Sprintf("computing repository path: %v", err))
	}
	return attachRepositorySuccess(attachRepositoryOutput{
		Status:       status,
		Repository:   repoURL,
		Path:         filepath.ToSlash(relPath),
		AbsolutePath: dest,
		BaseBranch:   baseBranch,
		BranchName:   branchName,
	})
}

func sameRepositoryURL(a, b string) bool {
	return canonicalRepositoryURL(a) == canonicalRepositoryURL(b)
}

func canonicalRepositoryURL(raw string) string {
	repo := strings.TrimSpace(raw)
	repo = strings.TrimSuffix(strings.TrimRight(repo, "/"), ".git")
	if repo == "" {
		return ""
	}

	if strings.HasPrefix(repo, "git@") {
		repo = strings.TrimPrefix(repo, "git@")
		if host, path, ok := strings.Cut(repo, ":"); ok {
			return strings.ToLower(host) + "/" + strings.TrimPrefix(path, "/")
		}
	}

	if strings.HasPrefix(repo, "ssh://git@") {
		repo = strings.TrimPrefix(repo, "ssh://git@")
		if host, path, ok := strings.Cut(repo, "/"); ok {
			return strings.ToLower(host) + "/" + strings.TrimPrefix(path, "/")
		}
	}

	parsed, err := url.Parse(repo)
	if err == nil && parsed.Scheme != "" && parsed.Host != "" {
		return strings.ToLower(parsed.Host) + "/" + strings.TrimPrefix(parsed.Path, "/")
	}

	if strings.HasPrefix(repo, "github.com/") {
		return "github.com/" + strings.TrimPrefix(repo, "github.com/")
	}
	return repo
}

func resolveRepositoryWorkDir(workDir, repoPath string) (string, error) {
	repoPath = strings.TrimSpace(repoPath)
	if repoPath == "" || repoPath == "." {
		return workDir, nil
	}
	resolved, err := pathutil.ResolveWorkspace(workDir, repoPath)
	if err != nil {
		return "", err
	}
	if !isGitRepository(resolved) {
		return "", fmt.Errorf("%s is not a git repository", repoPath)
	}
	return resolved, nil
}

func isGitRepository(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && info.IsDir()
}

func ensureGitInfoExclude(repoDir string, patterns ...string) error {
	if !isGitRepository(repoDir) {
		return nil
	}
	path := filepath.Join(repoDir, ".git", "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	existingBytes, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	existing := "\n" + string(existingBytes) + "\n"
	var missing []string
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if !strings.Contains(existing, "\n"+pattern+"\n") {
			missing = append(missing, pattern)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, pattern := range missing {
		if _, err := fmt.Fprintln(f, pattern); err != nil {
			return err
		}
	}
	return nil
}

func normalizeRepositoryInput(raw string) (repoURL, repoName string, err error) {
	repo := strings.TrimSpace(raw)
	if repo == "" {
		return "", "", fmt.Errorf("repository is required")
	}
	if strings.ContainsAny(repo, " \t\r\n") {
		return "", "", fmt.Errorf("repository must not contain whitespace")
	}

	if strings.HasPrefix(repo, "github.com/") {
		repo = "https://" + repo
	} else if isGitHubOwnerRepoShorthand(repo) {
		repo = "https://github.com/" + repo
	}
	name := repositoryNameFromURL(repo)
	if name == "" {
		return "", "", fmt.Errorf("could not derive repository name from %q", raw)
	}
	if strings.HasPrefix(repo, "https://github.com/") && !strings.HasSuffix(repo, ".git") {
		repo += ".git"
	}
	return repo, name, nil
}

func isGitHubOwnerRepoShorthand(repo string) bool {
	if strings.Count(repo, "/") != 1 || strings.Contains(repo, "://") || strings.HasPrefix(repo, "/") {
		return false
	}
	parts := strings.Split(repo, "/")
	return parts[0] != "" && parts[1] != "" && !strings.Contains(parts[0], ".")
}

func repositoryNameFromURL(repo string) string {
	trimmed := strings.TrimSuffix(strings.TrimRight(repo, "/"), ".git")
	if idx := strings.LastIndexAny(trimmed, "/:"); idx >= 0 {
		trimmed = trimmed[idx+1:]
	}
	return strings.TrimSpace(trimmed)
}

func sanitizeRepositoryAlias(raw string) string {
	raw = strings.TrimSpace(strings.TrimSuffix(raw, ".git"))
	if raw == "" {
		return ""
	}
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(raw) {
		allowed := r == '-' || r == '_' || r == '.' ||
			(r >= 'a' && r <= 'z') ||
			(r >= '0' && r <= '9')
		if allowed {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), ".-_")
	if len(out) > 80 {
		out = strings.Trim(out[:80], ".-_")
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func attachRepositorySuccess(out attachRepositoryOutput) (agentsdk.ToolResult, error) {
	b, _ := json.Marshal(out)
	return agentsdk.ToolResult{Content: string(b)}, nil
}

func attachRepositoryError(msg string) (agentsdk.ToolResult, error) {
	out := attachRepositoryOutput{Status: "error", Error: msg}
	b, _ := json.Marshal(out)
	return agentsdk.ToolResult{Content: string(b), IsError: true}, nil
}
