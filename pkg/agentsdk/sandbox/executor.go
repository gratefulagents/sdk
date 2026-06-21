package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk/policy"
)

const (
	SandboxModeEnv               = "GRATEFULAGENTS_COMMAND_SANDBOX"
	SandboxPathEnv               = "GRATEFULAGENTS_COMMAND_SANDBOX_PATH"
	SandboxPathPrependEnv        = "GRATEFULAGENTS_COMMAND_SANDBOX_PATH_PREPEND"
	SandboxPathAppendEnv         = "GRATEFULAGENTS_COMMAND_SANDBOX_PATH_APPEND"
	SandboxExtraReadOnlyPathsEnv = "GRATEFULAGENTS_COMMAND_SANDBOX_EXTRA_RO_PATHS"
	SandboxExtraWritablePathsEnv = "GRATEFULAGENTS_COMMAND_SANDBOX_EXTRA_RW_PATHS"
	SandboxExtraEnvEnv           = "GRATEFULAGENTS_COMMAND_SANDBOX_EXTRA_ENV"
	// SandboxAllowUnsafeReadOnlyLocalEnv opts a host into running read-only
	// commands through the advisory LocalExecutor when the enforcing subprocess
	// sandbox is unavailable on this platform (e.g. macOS/Windows dev hosts,
	// where bubblewrap does not exist). It is a developer convenience, not a
	// security boundary, so it defaults off and must be set explicitly.
	SandboxAllowUnsafeReadOnlyLocalEnv = "GRATEFULAGENTS_COMMAND_SANDBOX_ALLOW_UNSAFE_READONLY_LOCAL"

	sandboxModeAuto     = "auto"
	sandboxModeDisabled = "disabled"
	sandboxModeRequired = "required"
)

var defaultSandboxPathEntries = []string{
	"/opt/gratefulagents/bin",
	"/opt/conda/bin",
	"/opt/flutter/bin",
	"/opt/android-sdk/cmdline-tools/latest/bin",
	"/opt/android-sdk/platform-tools",
	"/nix/var/nix/profiles/default/bin",
	"/usr/local/go/bin",
	"/usr/local/sbin",
	"/usr/local/bin",
	"/usr/sbin",
	"/usr/bin",
	"/sbin",
	"/bin",
	"/tmp/home/.local/bin",
	"/tmp/npm-global/bin",
	"/tmp/pnpm",
	"/tmp/go/bin",
	"/tmp/cargo/bin",
	"/tmp/gems/bin",
	"/tmp/bun/bin",
	"/tmp/dotnet/tools",
	"/workspace/.cache/go/bin",
}

var defaultSandboxReadOnlyPaths = []string{
	"/usr",
	"/bin",
	"/lib",
	"/lib64",
	"/sbin",
	"/etc",
	"/opt/gratefulagents",
	"/opt/conda",
	"/opt/flutter",
	"/opt/android-sdk",
	"/nix",
}

var deniedSandboxReadOnlyPathPrefixes = []string{
	"/home",
	"/root",
	"/run",
	"/var/lib",
	"/var/log",
	"/var/run",
	"/var/spool",
	"/var/tmp",
}

// Config controls subprocess sandbox behavior. Hosts should populate this from
// their own configuration layer before constructing an executor.
type Config struct {
	Mode                string
	RunningInKubernetes bool
	WorkspaceRoot       string
	Path                string
	PathPrepend         []string
	PathAppend          []string
	ExtraReadOnlyPaths  []string
	ExtraWritablePaths  []string
	ExtraEnv            map[string]string
	GOROOT              string
	// AllowUnsafeReadOnlyLocal lets a host explicitly skip the enforcing
	// subprocess sandbox for read-only requests when Mode is disabled. This is
	// a compatibility escape hatch, not a security boundary.
	AllowUnsafeReadOnlyLocal bool
}

// SandboxConfigEnvNames returns host-propagated environment variables that
// tune the subprocess sandbox. Extra writable paths should be limited to
// explicit scratch directories because they intentionally expand the write
// boundary beyond the workspace.
func SandboxConfigEnvNames() []string {
	return []string{
		SandboxPathEnv,
		SandboxPathPrependEnv,
		SandboxPathAppendEnv,
		SandboxExtraReadOnlyPathsEnv,
		SandboxExtraWritablePathsEnv,
		SandboxExtraEnvEnv,
		SandboxAllowUnsafeReadOnlyLocalEnv,
	}
}

// ConfigFromEnv converts the SDK sandbox environment contract into an explicit
// Config. Hosts can either call this directly or rely on Default, which uses
// it for backwards-compatible worker-pod configuration.
func ConfigFromEnv() Config {
	return Config{
		Mode:                     os.Getenv(SandboxModeEnv),
		Path:                     os.Getenv(SandboxPathEnv),
		PathPrepend:              splitPathList(os.Getenv(SandboxPathPrependEnv)),
		PathAppend:               splitPathList(os.Getenv(SandboxPathAppendEnv)),
		ExtraReadOnlyPaths:       splitPathList(os.Getenv(SandboxExtraReadOnlyPathsEnv)),
		ExtraWritablePaths:       splitPathList(os.Getenv(SandboxExtraWritablePathsEnv)),
		ExtraEnv:                 sandboxExtraEnvFromEnv(os.Getenv(SandboxExtraEnvEnv)),
		GOROOT:                   os.Getenv("GOROOT"),
		AllowUnsafeReadOnlyLocal: envFlag(os.Getenv(SandboxAllowUnsafeReadOnlyLocalEnv)),
	}
}

// Request describes one untrusted command process tree.
type Request struct {
	Argv           []string
	WorkDir        string
	PermissionMode policy.PermissionMode
	Timeout        time.Duration
	Env            map[string]string
}

// Result is the combined stdout/stderr result of a command run.
type Result struct {
	Output   string
	ExitCode int
	TimedOut bool
	Capped   bool
}

// Executor builds and runs untrusted command process trees.
type Executor interface {
	Build(ctx context.Context, req Request) (*exec.Cmd, error)
	Run(ctx context.Context, req Request) (Result, error)
}

// Default returns the production command executor with deterministic SDK
// defaults. Read-only command runs require the subprocess sandbox; local
// write-capable development falls back to a sanitized process unless
// sandboxing is explicitly required.
func Default() Executor {
	return defaultExecutor{config: ConfigFromEnv()}
}

// DefaultWithConfig returns the production command executor with host-supplied
// sandbox configuration.
func DefaultWithConfig(config Config) Executor {
	return defaultExecutor{config: config}
}

type defaultExecutor struct {
	config Config
}

func (e defaultExecutor) Build(ctx context.Context, req Request) (*exec.Cmd, error) {
	config := normalizeConfig(e.config)
	mode := strings.ToLower(strings.TrimSpace(config.Mode))
	if mode == "" {
		mode = sandboxModeAuto
	}

	switch mode {
	case sandboxModeDisabled:
		return LocalExecutor{Config: config}.Build(ctx, req)
	case sandboxModeRequired:
		return BubblewrapExecutor{Config: config}.Build(ctx, req)
	case sandboxModeAuto:
		if config.RunningInKubernetes || policy.NormalizePermissionMode(string(req.PermissionMode)) == policy.PermissionModeReadOnly {
			// Read-only and in-cluster workloads need the enforcing subprocess
			// sandbox. When it is unavailable on this platform (no bubblewrap —
			// e.g. macOS/Windows dev hosts) and the host explicitly opted into
			// advisory local execution, fall back to LocalExecutor so commands
			// can still run. Otherwise fail closed via the BubblewrapExecutor,
			// which returns a descriptive "requires linux" error.
			if subprocessSandboxAvailable() || !config.AllowUnsafeReadOnlyLocal {
				return BubblewrapExecutor{Config: config}.Build(ctx, req)
			}
			return LocalExecutor{Config: config}.Build(ctx, req)
		}
		return LocalExecutor{Config: config}.Build(ctx, req)
	default:
		return nil, fmt.Errorf("unsupported sandbox mode %q", mode)
	}
}

func (e defaultExecutor) Run(ctx context.Context, req Request) (Result, error) {
	cmd, err := e.Build(ctx, req)
	if err != nil {
		return Result{ExitCode: -1}, err
	}
	return runBuiltCommand(ctx, cmd, req.Timeout)
}

func normalizeConfig(config Config) Config {
	config.Mode = strings.TrimSpace(config.Mode)
	config.WorkspaceRoot = strings.TrimSpace(config.WorkspaceRoot)
	config.Path = strings.TrimSpace(config.Path)
	config.GOROOT = strings.TrimSpace(config.GOROOT)
	return config
}

// subprocessSandboxAvailable reports whether the enforcing bubblewrap sandbox
// can run on this platform. It is currently linux-only; on other platforms the
// BubblewrapExecutor fails with a "requires linux" error.
func subprocessSandboxAvailable() bool {
	return runtime.GOOS == "linux"
}

// envFlag parses a boolean-ish environment value ("1", "true", "yes", "on").
func envFlag(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if b, err := strconv.ParseBool(value); err == nil {
		return b
	}
	switch strings.ToLower(value) {
	case "yes", "y", "on":
		return true
	default:
		return false
	}
}

// LocalExecutor is the compatibility fallback. It never inherits the parent
// environment, but it does not provide a filesystem or /proc boundary, and
// therefore CANNOT enforce read-only permission modes. It is advisory-only
// for non-readonly workloads; requests with PermissionMode == ReadOnly are
// rejected so that callers cannot silently downgrade an enforcement boundary
// when no real sandbox executor is available.
type LocalExecutor struct {
	Config Config
}

func (e LocalExecutor) Build(ctx context.Context, req Request) (*exec.Cmd, error) {
	if err := validateRequest(req); err != nil {
		return nil, err
	}
	if policy.NormalizePermissionMode(string(req.PermissionMode)) == policy.PermissionModeReadOnly && !e.Config.AllowUnsafeReadOnlyLocal {
		return nil, errors.New("LocalExecutor cannot enforce read-only permission mode; subprocess sandbox required")
	}
	cmd := exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...)
	cmd.Dir = req.WorkDir
	cmd.Env = SafeEnvWithConfig(req.Env, e.Config)
	return cmd, nil
}

func (e LocalExecutor) Run(ctx context.Context, req Request) (Result, error) {
	cmd, err := e.Build(ctx, req)
	if err != nil {
		return Result{ExitCode: -1}, err
	}
	return runBuiltCommand(ctx, cmd, req.Timeout)
}

// BubblewrapExecutor runs commands in a same-pod subprocess sandbox.
type BubblewrapExecutor struct {
	Binary string
	Config Config
}

func (e BubblewrapExecutor) Build(ctx context.Context, req Request) (*exec.Cmd, error) {
	if err := validateRequest(req); err != nil {
		return nil, err
	}
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("subprocess sandbox requires linux, got %s", runtime.GOOS)
	}

	binary := strings.TrimSpace(e.Binary)
	if binary == "" {
		binary = "bwrap"
	}
	resolved, err := exec.LookPath(binary)
	if err != nil {
		return nil, fmt.Errorf("subprocess sandbox binary %q not found: %w", binary, err)
	}

	args, err := BubblewrapArgsWithConfig(req, e.Config)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, resolved, args...)
	cmd.Dir = "/"
	cmd.Env = SafeEnvWithConfig(nil, e.Config)
	return cmd, nil
}

func (e BubblewrapExecutor) Run(ctx context.Context, req Request) (Result, error) {
	cmd, err := e.Build(ctx, req)
	if err != nil {
		return Result{ExitCode: -1}, err
	}
	return runBuiltCommand(ctx, cmd, req.Timeout)
}

// BubblewrapArgs returns the bwrap argument vector for tests and diagnostics.
func BubblewrapArgs(req Request) ([]string, error) {
	return BubblewrapArgsWithConfig(req, Config{})
}

// BubblewrapArgsWithConfig returns the bwrap argument vector using explicit
// sandbox configuration.
func BubblewrapArgsWithConfig(req Request, config Config) ([]string, error) {
	if err := validateRequest(req); err != nil {
		return nil, err
	}
	config = normalizeConfig(config)

	workDir, err := filepath.Abs(req.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("absolute workdir: %w", err)
	}
	workspaceRoot, err := workspaceRootFor(workDir, config.WorkspaceRoot)
	if err != nil {
		return nil, err
	}
	mode := policy.NormalizePermissionMode(string(req.PermissionMode))

	args := []string{
		"--die-with-parent",
		"--unshare-user",
		"--unshare-pid",
		"--unshare-ipc",
		"--unshare-uts",
		"--unshare-cgroup-try",
		"--uid", fmt.Sprintf("%d", os.Getuid()),
		"--gid", fmt.Sprintf("%d", os.Getgid()),
		"--clearenv",
	}
	for _, pair := range SafeEnvWithConfig(req.Env, config) {
		key, val, _ := strings.Cut(pair, "=")
		args = append(args, "--setenv", key, val)
	}
	args = append(args,
		"--dev", "/dev",
		"--tmpfs", "/tmp",
		"--dir", "/tmp/home",
		"--dir", "/proc",
	)

	for _, path := range sandboxReadOnlyPaths(workspaceRoot, config) {
		if pathExists(path) {
			args = append(args, "--ro-bind", path, path)
		}
	}
	writablePaths := existingPaths(sandboxWritablePaths(workspaceRoot, config))
	args = append(args, bindMountPointDirs(writablePaths)...)
	for _, path := range writablePaths {
		args = append(args, "--bind", path, path)
	}

	args = append(args, mountPointDirs(workspaceRoot)...)
	if mode == policy.PermissionModeReadOnly {
		args = append(args, "--ro-bind", workspaceRoot, workspaceRoot)
	} else {
		args = append(args, "--bind", workspaceRoot, workspaceRoot)
	}

	args = append(args, "--chdir", workDir, "--")
	args = append(args, req.Argv...)
	return args, nil
}

func validateRequest(req Request) error {
	if len(req.Argv) == 0 || strings.TrimSpace(req.Argv[0]) == "" {
		return errors.New("command argv is required")
	}
	if strings.TrimSpace(req.WorkDir) == "" {
		return errors.New("command workdir is required")
	}
	return nil
}

func runBuiltCommand(ctx context.Context, cmd *exec.Cmd, timeout time.Duration) (Result, error) {
	runCtx := ctx
	cancel := func() {}
	if timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()

	// Reconstruct the command without context-driven auto-kill so we can manage
	// process-group termination ourselves: the default exec.CommandContext kill
	// only signals the leader, letting forked/daemonized children escape the
	// timeout. We re-issue the command via exec.Command and configure the OS
	// process group below.
	fresh := exec.Command(cmd.Path, cmd.Args[1:]...)
	fresh.Dir = cmd.Dir
	fresh.Env = append([]string(nil), cmd.Env...)
	configureProcessGroup(fresh)

	output := newBoundedOutput(maxExecutorOutputBytes)
	fresh.Stdout = output
	fresh.Stderr = output

	if err := fresh.Start(); err != nil {
		return Result{ExitCode: -1}, err
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- fresh.Wait() }()

	timedOut := false
	var runErr error
	select {
	case runErr = <-waitDone:
	case <-runCtx.Done():
		timedOut = errors.Is(runCtx.Err(), context.DeadlineExceeded)
		terminateProcessGroup(fresh.Process)
		select {
		case runErr = <-waitDone:
		case <-time.After(sandboxKillGrace):
			killProcessGroup(fresh.Process)
			runErr = <-waitDone
		}
	}

	result := Result{Output: string(output.Bytes()), ExitCode: 0, Capped: output.Capped()}
	if result.Capped {
		result.Output += executorTruncationNotice
	}
	if timedOut {
		result.TimedOut = true
		result.ExitCode = -1
		return result, nil
	}
	if runErr == nil {
		return result, nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}
	result.ExitCode = -1
	return result, runErr
}

const (
	maxExecutorOutputBytes   = 1024 * 1024
	executorTruncationNotice = "\n[output truncated: captured head/tail after exceeding 1MB cap; process was not terminated]"
)

type boundedOutput struct {
	mu     sync.Mutex
	buf    []byte
	head   []byte
	tail   []byte
	cap    int
	capped bool
}

func newBoundedOutput(cap int) *boundedOutput {
	return &boundedOutput{cap: cap}
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.cap <= 0 {
		b.capped = true
		return len(p), nil
	}
	if !b.capped && len(b.buf)+len(p) <= b.cap {
		b.buf = append(b.buf, p...)
		return len(p), nil
	}
	if !b.capped {
		combined := make([]byte, 0, len(b.buf)+len(p))
		combined = append(combined, b.buf...)
		combined = append(combined, p...)
		b.buf = nil
		b.capped = true
		b.head, b.tail = splitBoundedHeadTail(combined, b.cap)
		return len(p), nil
	}
	_, tailLimit := boundedHeadTailLimits(b.cap)
	b.tail = appendBoundedTail(b.tail, p, tailLimit)
	return len(p), nil
}

func (b *boundedOutput) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.capped {
		out := make([]byte, 0, len(b.head)+len(executorTruncationMarker)+len(b.tail))
		out = append(out, b.head...)
		out = append(out, executorTruncationMarker...)
		out = append(out, b.tail...)
		return out
	}
	out := make([]byte, len(b.buf))
	copy(out, b.buf)
	return out
}

func (b *boundedOutput) Capped() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.capped
}

const executorTruncationMarker = "\n[output truncated: middle omitted]\n"

func boundedHeadTailLimits(cap int) (int, int) {
	if cap <= 1 {
		return cap, 0
	}
	head := cap / 2
	return head, cap - head
}

func splitBoundedHeadTail(data []byte, cap int) ([]byte, []byte) {
	headLimit, tailLimit := boundedHeadTailLimits(cap)
	head := append([]byte(nil), data[:minInt(len(data), headLimit)]...)
	var tail []byte
	if tailLimit > 0 {
		start := len(data) - tailLimit
		if start < 0 {
			start = 0
		}
		tail = append([]byte(nil), data[start:]...)
	}
	return head, tail
}

func appendBoundedTail(tail, p []byte, limit int) []byte {
	if limit <= 0 {
		return nil
	}
	tail = append(tail, p...)
	if len(tail) <= limit {
		return tail
	}
	out := make([]byte, limit)
	copy(out, tail[len(tail)-limit:])
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// workspaceRootFor resolves the workspace root that bwrap should bind into
// the sandbox. Both the workDir and the configured root are resolved via
// filepath.EvalSymlinks before checking containment, so that workDirs
// containing `..` segments or symlinks pointing outside the configured root
// cannot escape it. When a configuredRoot is supplied and the workDir does
// not resolve inside it, an error is returned rather than silently widening
// the sandbox.
func workspaceRootFor(workDir, configuredRoot string) (string, error) {
	resolvedWorkDir := resolveExistingPrefix(workDir)

	if configured := strings.TrimSpace(configuredRoot); configured != "" {
		abs, err := filepath.Abs(configured)
		if err != nil {
			return "", fmt.Errorf("absolute workspace root: %w", err)
		}
		resolvedRoot := resolveExistingPrefix(abs)
		if resolvedWorkDir == resolvedRoot || strings.HasPrefix(resolvedWorkDir, resolvedRoot+string(os.PathSeparator)) {
			return resolvedRoot, nil
		}
		return "", fmt.Errorf("workdir %q resolves outside configured workspace root %q", resolvedWorkDir, resolvedRoot)
	}

	if strings.HasPrefix(resolvedWorkDir, "/workspace/") || resolvedWorkDir == "/workspace" {
		return "/workspace", nil
	}
	return resolvedWorkDir, nil
}

// resolveExistingPrefix returns filepath.EvalSymlinks(path) when the full
// path exists, otherwise it walks up to the deepest existing ancestor,
// resolves that, and reattaches the unresolved trailing components. This
// lets workspace-root containment checks handle workdirs whose leaf
// directory has not been created yet without silently bypassing symlink
// resolution for the components that *do* exist.
func resolveExistingPrefix(path string) string {
	clean := filepath.Clean(path)
	if r, err := filepath.EvalSymlinks(clean); err == nil {
		return filepath.Clean(r)
	}
	parent := filepath.Dir(clean)
	if parent == clean {
		return clean
	}
	return filepath.Join(resolveExistingPrefix(parent), filepath.Base(clean))
}

func mountPointDirs(path string) []string {
	clean := filepath.Clean(path)
	if clean == string(os.PathSeparator) {
		return nil
	}
	var dirs []string
	for _, part := range strings.Split(strings.TrimPrefix(clean, string(os.PathSeparator)), string(os.PathSeparator)) {
		if part == "" {
			continue
		}
		if len(dirs) == 0 {
			dirs = append(dirs, string(os.PathSeparator)+part)
		} else {
			dirs = append(dirs, filepath.Join(dirs[len(dirs)-1], part))
		}
	}
	args := make([]string, 0, len(dirs)*2)
	for _, dir := range dirs {
		args = append(args, "--dir", dir)
	}
	return args
}

func bindMountPointDirs(paths []string) []string {
	seen := map[string]struct{}{
		"/tmp": {},
	}
	var args []string
	for _, path := range paths {
		for _, dir := range mountPointDirPaths(path) {
			if _, ok := seen[dir]; ok {
				continue
			}
			seen[dir] = struct{}{}
			args = append(args, "--dir", dir)
		}
	}
	return args
}

func mountPointDirPaths(path string) []string {
	clean := filepath.Clean(path)
	if clean == string(os.PathSeparator) {
		return nil
	}
	var dirs []string
	for _, part := range strings.Split(strings.TrimPrefix(clean, string(os.PathSeparator)), string(os.PathSeparator)) {
		if part == "" {
			continue
		}
		if len(dirs) == 0 {
			dirs = append(dirs, string(os.PathSeparator)+part)
		} else {
			dirs = append(dirs, filepath.Join(dirs[len(dirs)-1], part))
		}
	}
	return dirs
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func existingPaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		if pathExists(path) {
			out = append(out, path)
		}
	}
	return out
}

func sandboxReadOnlyPaths(workspaceRoot string, config Config) []string {
	paths := make([]string, 0, len(defaultSandboxReadOnlyPaths))
	for _, path := range defaultSandboxReadOnlyPaths {
		paths = appendSandboxReadOnlyPath(paths, path, workspaceRoot)
	}
	for _, path := range config.ExtraReadOnlyPaths {
		paths = appendSandboxReadOnlyPath(paths, path, workspaceRoot)
	}
	return paths
}

func sandboxWritablePaths(workspaceRoot string, config Config) []string {
	var paths []string
	for _, path := range config.ExtraWritablePaths {
		paths = appendSandboxWritablePath(paths, path, workspaceRoot)
	}
	return paths
}

func appendSandboxWritablePath(paths []string, path, workspaceRoot string) []string {
	clean := cleanAbsolutePath(path)
	if clean == "" || isForbiddenSandboxWritablePath(clean, workspaceRoot) {
		return paths
	}
	for _, existing := range paths {
		if clean == existing || isPathWithin(clean, existing) {
			return paths
		}
	}
	return append(paths, clean)
}

func appendSandboxReadOnlyPath(paths []string, path, workspaceRoot string) []string {
	clean := cleanAbsolutePath(path)
	if clean == "" || isForbiddenSandboxReadOnlyPath(clean, workspaceRoot) {
		return paths
	}
	for _, existing := range paths {
		if clean == existing || isPathWithin(clean, existing) {
			return paths
		}
	}
	return append(paths, clean)
}

func isForbiddenSandboxReadOnlyPath(path, workspaceRoot string) bool {
	if path == "" || path == string(os.PathSeparator) || path == "/tmp" || path == "/proc" || path == "/dev" {
		return true
	}
	for _, root := range []string{"/opt", "/var"} {
		if path == root {
			return true
		}
	}
	if workspaceRoot != "" && isPathWithin(path, filepath.Clean(workspaceRoot)) {
		return true
	}
	for _, prefix := range deniedSandboxReadOnlyPathPrefixes {
		if isPathWithin(path, prefix) {
			return true
		}
	}
	return false
}

func isForbiddenSandboxWritablePath(path, workspaceRoot string) bool {
	if path == "" || path == string(os.PathSeparator) || path == "/tmp" || path == "/proc" || path == "/dev" {
		return true
	}
	for _, root := range []string{"/etc", "/opt", "/usr", "/bin", "/lib", "/lib64", "/sbin", "/nix", "/var", "/home", "/root", "/run", "/boot", "/sys"} {
		if path == root || isPathWithin(path, root) {
			return true
		}
	}
	if workspaceRoot != "" && isPathWithin(path, filepath.Clean(workspaceRoot)) {
		return true
	}
	return false
}

func sandboxPath(config Config) string {
	if configured := strings.TrimSpace(config.Path); configured != "" {
		if path := cleanPathList(configured); path != "" {
			return path
		}
	}

	entries := append([]string(nil), defaultSandboxPathEntries...)
	if prepend := cleanPathEntryList(config.PathPrepend); len(prepend) > 0 {
		entries = append(prepend, entries...)
	}
	if appendEntries := cleanPathEntryList(config.PathAppend); len(appendEntries) > 0 {
		entries = append(entries, appendEntries...)
	}
	return cleanPathEntries(entries)
}

func cleanPathList(value string) string {
	return cleanPathEntries(splitPathList(value))
}

func cleanPathEntries(entries []string) string {
	cleaned := make([]string, 0, len(entries))
	seen := map[string]struct{}{}
	for _, entry := range entries {
		clean := cleanAbsolutePath(entry)
		if clean == "" {
			continue
		}
		if _, ok := seen[clean]; ok {
			continue
		}
		seen[clean] = struct{}{}
		cleaned = append(cleaned, clean)
	}
	return strings.Join(cleaned, string(os.PathListSeparator))
}

func splitPathList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := filepath.SplitList(value)
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if clean := cleanAbsolutePath(part); clean != "" {
			out = append(out, clean)
		}
	}
	return out
}

func sandboxExtraEnvFromEnv(value string) map[string]string {
	pairs := splitSandboxEnv(value)
	if len(pairs) == 0 {
		return nil
	}
	out := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		key, val, ok := strings.Cut(pair, "=")
		key = strings.TrimSpace(key)
		if !ok || !validEnvKey(key) {
			continue
		}
		out[key] = strings.TrimSpace(val)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func splitSandboxEnv(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	if strings.Contains(value, "\n") {
		return strings.Split(value, "\n")
	}
	return strings.Split(value, ",")
}

func cleanPathEntryList(entries []string) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if clean := cleanAbsolutePath(entry); clean != "" {
			out = append(out, clean)
		}
	}
	return out
}

func cleanAbsolutePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" || !filepath.IsAbs(path) {
		return ""
	}
	return filepath.Clean(path)
}

func isPathWithin(path, root string) bool {
	if root == "" || root == string(os.PathSeparator) {
		return false
	}
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	return path == root || strings.HasPrefix(path, root+string(os.PathSeparator))
}

// SafeEnv returns the only environment passed to untrusted subprocesses.
func SafeEnv(overrides map[string]string) []string {
	return SafeEnvWithConfig(overrides, Config{})
}

// SafeEnvWithConfig returns the only environment passed to untrusted
// subprocesses, using explicit sandbox configuration.
func SafeEnvWithConfig(overrides map[string]string, config Config) []string {
	base := SafeEnvMapWithConfig(config)
	for key, val := range overrides {
		k := strings.TrimSpace(key)
		if !validEnvKey(k) {
			continue
		}
		base[k] = ExpandSafe(val, base)
	}

	keys := make([]string, 0, len(base))
	for key := range base {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+base[key])
	}
	return env
}

// SafeEnvMap returns the deterministic base environment for subprocesses.
func SafeEnvMap() map[string]string {
	return SafeEnvMapWithConfig(Config{})
}

// SafeEnvMapWithConfig returns the deterministic base environment for
// subprocesses using explicit sandbox configuration.
func SafeEnvMapWithConfig(config Config) map[string]string {
	config = normalizeConfig(config)
	env := map[string]string{
		"PATH":                sandboxPath(config),
		"HOME":                "/tmp/home",
		"TMPDIR":              "/tmp",
		"LANG":                "C.UTF-8",
		"LC_ALL":              "C.UTF-8",
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_CONFIG_GLOBAL":   "/dev/null",
		"GIT_CONFIG_NOSYSTEM": "1",
		"XDG_CACHE_HOME":      "/tmp/.cache",
		"XDG_CONFIG_HOME":     "/tmp/.config",
		"XDG_DATA_HOME":       "/tmp/.local/share",
		"XDG_STATE_HOME":      "/tmp/.local/state",
		"GOPATH":              "/tmp/go",
		"GOBIN":               "/tmp/go/bin",
		"GOMODCACHE":          "/tmp/go/pkg/mod",
		"GOCACHE":             "/tmp/go-build",
		"GOTOOLCHAIN":         "local",
		"GO_TELEMETRY_CHILD":  "2",
		"NPM_CONFIG_CACHE":    "/tmp/npm-cache",
		"NPM_CONFIG_PREFIX":   "/tmp/npm-global",
		"PNPM_HOME":           "/tmp/pnpm",
		"COREPACK_HOME":       "/tmp/corepack",
		"YARN_CACHE_FOLDER":   "/tmp/yarn-cache",
		"PIP_CACHE_DIR":       "/tmp/pip-cache",
		"PIPX_HOME":           "/tmp/pipx",
		"PIPX_BIN_DIR":        "/tmp/home/.local/bin",
		"UV_CACHE_DIR":        "/tmp/uv-cache",
		"CARGO_HOME":          "/tmp/cargo",
		"RUSTUP_HOME":         "/tmp/rustup",
		"MIX_HOME":            "/tmp/mix",
		"HEX_HOME":            "/tmp/hex",
		"REBAR_CACHE_DIR":     "/tmp/rebar-cache",
		"GRADLE_USER_HOME":    "/tmp/gradle",
		"DOTNET_CLI_HOME":     "/tmp/dotnet",
		"NUGET_PACKAGES":      "/tmp/nuget",
		"GEM_HOME":            "/tmp/gems",
		"GEM_PATH":            "/tmp/gems",
		"BUNDLE_USER_HOME":    "/tmp/bundle-home",
		"BUNDLE_PATH":         "/tmp/bundle",
		"COMPOSER_HOME":       "/tmp/composer",
		"DENO_DIR":            "/tmp/deno",
		"BUN_INSTALL":         "/tmp/bun",
	}
	if goroot := sandboxGOROOT(config); goroot != "" {
		env["GOROOT"] = goroot
	}
	applySandboxExtraEnv(env, config)
	return env
}

func sandboxGOROOT(config Config) string {
	if configured := strings.TrimSpace(config.GOROOT); configured != "" && filepath.IsAbs(configured) {
		return filepath.Clean(configured)
	}
	if pathExists("/usr/local/go") {
		return "/usr/local/go"
	}
	return ""
}

func applySandboxExtraEnv(env map[string]string, config Config) {
	keys := make([]string, 0, len(config.ExtraEnv))
	for key := range config.ExtraEnv {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := config.ExtraEnv[key]
		key = strings.TrimSpace(key)
		if !validEnvKey(key) {
			continue
		}
		env[key] = ExpandSafe(strings.TrimSpace(value), env)
	}
}

func validEnvKey(key string) bool {
	if key == "" {
		return false
	}
	for i, r := range key {
		switch {
		case r == '_':
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	first := key[0]
	return first == '_' || (first >= 'A' && first <= 'Z') || (first >= 'a' && first <= 'z')
}

// ExpandSafe expands variables from the provided safe environment only.
func ExpandSafe(value string, env map[string]string) string {
	return os.Expand(value, func(key string) string {
		return env[key]
	})
}
