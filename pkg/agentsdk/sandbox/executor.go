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
	// SandboxExposeKubernetesServiceAccountEnv allows an explicitly privileged
	// host to expose the pod's projected service-account credentials. Keep this
	// opt-in: ordinary agent subprocesses must not inherit Kubernetes identity.
	SandboxExposeKubernetesServiceAccountEnv = "GRATEFULAGENTS_COMMAND_SANDBOX_EXPOSE_KUBERNETES_SERVICE_ACCOUNT"
	// SandboxAllowUnsafeReadOnlyLocalEnv is retained for configuration parsing
	// compatibility. Restricted modes no longer honor it: they always require an
	// enforcing subprocess sandbox.
	SandboxAllowUnsafeReadOnlyLocalEnv = "GRATEFULAGENTS_COMMAND_SANDBOX_ALLOW_UNSAFE_READONLY_LOCAL"

	sandboxModeAuto     = "auto"
	sandboxModeDisabled = "disabled"
	sandboxModeRequired = "required"
)

// inheritedSystemEnvVars lists the only parent-process environment variables
// that untrusted subprocesses inherit. Everything else must be granted
// explicitly through Config.ExtraEnv or per-request overrides, so secrets in
// the host process environment can never leak implicitly. LC_* locale
// variables are additionally inherited by prefix.
var inheritedSystemEnvVars = []string{
	"PATH",
	"HOME",
	"USER",
	"LOGNAME",
	"SHELL",
	"TERM",
	"COLORTERM",
	"LANG",
	"TZ",
	"TMPDIR",
}

// fallbackPathEntries keeps the child usable when the parent PATH is empty
// and no PATH configuration is supplied.
var fallbackPathEntries = []string{"/usr/local/bin", "/usr/bin", "/bin"}

// Config controls subprocess sandbox behavior. Hosts should populate this from
// their own configuration layer before constructing an executor.
type Config struct {
	Mode                string
	RunningInKubernetes bool
	WorkspaceRoot       string
	// Path fully replaces the inherited PATH when set. PathPrepend/PathAppend
	// adjust the inherited (or configured) PATH instead.
	Path               string
	PathPrepend        []string
	PathAppend         []string
	ExtraReadOnlyPaths []string
	ExtraWritablePaths []string
	ExtraEnv           map[string]string
	GOROOT             string
	// ExposeKubernetesServiceAccount leaves the pod's projected service-account
	// directory readable. Only trusted hosts should enable this for runs that
	// intentionally carry Kubernetes credentials.
	ExposeKubernetesServiceAccount bool
	// AllowUnsafeReadOnlyLocal is retained for configuration compatibility but
	// is not honored by executors; restricted modes always fail closed without
	// OS-enforced containment.
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
		SandboxExposeKubernetesServiceAccountEnv,
		SandboxAllowUnsafeReadOnlyLocalEnv,
	}
}

// ConfigFromEnv converts the SDK sandbox environment contract into an explicit
// Config. Hosts can either call this directly or rely on Default, which uses
// it for backwards-compatible worker-pod configuration.
func ConfigFromEnv() Config {
	return Config{
		Mode:                           os.Getenv(SandboxModeEnv),
		Path:                           os.Getenv(SandboxPathEnv),
		PathPrepend:                    splitPathList(os.Getenv(SandboxPathPrependEnv)),
		PathAppend:                     splitPathList(os.Getenv(SandboxPathAppendEnv)),
		ExtraReadOnlyPaths:             splitPathList(os.Getenv(SandboxExtraReadOnlyPathsEnv)),
		ExtraWritablePaths:             splitPathList(os.Getenv(SandboxExtraWritablePathsEnv)),
		ExtraEnv:                       sandboxExtraEnvFromEnv(os.Getenv(SandboxExtraEnvEnv)),
		GOROOT:                         os.Getenv("GOROOT"),
		ExposeKubernetesServiceAccount: envFlag(os.Getenv(SandboxExposeKubernetesServiceAccountEnv)),
		AllowUnsafeReadOnlyLocal:       envFlag(os.Getenv(SandboxAllowUnsafeReadOnlyLocalEnv)),
	}
}

// Request describes one untrusted command process tree.
type Request struct {
	Argv           []string
	WorkDir        string
	PermissionMode policy.PermissionMode
	Timeout        time.Duration
	Env            map[string]string
	// AllowNetwork keeps the host network namespace for a restricted request.
	// Restricted sandboxes otherwise unshare networking, leaving only loopback;
	// danger-full-access retains the host network by definition. Hosts should set
	// this only for tools with an explicit egress policy.
	AllowNetwork bool
	// WritablePaths grants request-scoped writable mounts outside the workspace.
	// Only trusted tool implementations may populate this field; read-only
	// requests always ignore it.
	WritablePaths []string
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

// FilesystemEnforcer is optionally implemented by executors that can report
// whether requests with a given permission mode run under an enforcing OS
// filesystem sandbox. Tool layers use it to skip textual command heuristics
// that would double-guard what the OS boundary already contains.
type FilesystemEnforcer interface {
	EnforcesFilesystem(mode policy.PermissionMode) bool
}

// ExecutorEnforcesFilesystem reports whether executor runs commands with the
// given permission mode under an enforcing OS filesystem sandbox. Executors
// that do not report enforcement are assumed advisory-only.
func ExecutorEnforcesFilesystem(executor Executor, mode policy.PermissionMode) bool {
	enforcer, ok := executor.(FilesystemEnforcer)
	return ok && enforcer.EnforcesFilesystem(mode)
}

// DefaultEnforcesFilesystem reports whether the environment-configured default
// executor runs write-capable commands inside the enforcing subprocess sandbox
// (true in worker pods, where GRATEFULAGENTS_COMMAND_SANDBOX=required).
func DefaultEnforcesFilesystem() bool {
	return defaultExecutor{config: ConfigFromEnv()}.EnforcesFilesystem(policy.PermissionModeWorkspaceWrite)
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
		return platformSandboxExecutor(config).Build(ctx, req)
	case sandboxModeAuto:
		permissionMode := policy.NormalizePermissionMode(string(req.PermissionMode))
		if permissionMode != policy.PermissionModeDangerFullAccess || config.RunningInKubernetes {
			// Every restricted mode requires OS-enforced containment. Backend
			// unavailability fails closed; local execution cannot safely enforce a
			// filesystem permission boundary.
			return platformSandboxExecutor(config).Build(ctx, req)
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

// EnforcesFilesystem mirrors the executor-selection logic in Build: it reports
// true exactly when a request with the given mode would run under an enforcing
// subprocess sandbox supported on this platform.
func (e defaultExecutor) EnforcesFilesystem(mode policy.PermissionMode) bool {
	config := normalizeConfig(e.config)
	switch strings.ToLower(strings.TrimSpace(config.Mode)) {
	case sandboxModeDisabled:
		return false
	case sandboxModeRequired:
		return platformSandboxAvailable()
	case "", sandboxModeAuto:
		if !platformSandboxAvailable() {
			return false
		}
		return config.RunningInKubernetes ||
			policy.NormalizePermissionMode(string(mode)) != policy.PermissionModeDangerFullAccess
	default:
		return false
	}
}

func normalizeConfig(config Config) Config {
	config.Mode = strings.TrimSpace(config.Mode)
	config.WorkspaceRoot = strings.TrimSpace(config.WorkspaceRoot)
	config.Path = strings.TrimSpace(config.Path)
	config.GOROOT = strings.TrimSpace(config.GOROOT)
	return config
}

func platformSandboxAvailable() bool {
	switch runtime.GOOS {
	case "linux":
		return true
	case "darwin":
		return seatbeltAvailable()
	default:
		return false
	}
}

func platformSandboxExecutor(config Config) Executor {
	switch runtime.GOOS {
	case "linux":
		return BubblewrapExecutor{Config: config}
	case "darwin":
		return SeatbeltExecutor{Config: config}
	default:
		return unavailableSandboxExecutor{goos: runtime.GOOS}
	}
}

type unavailableSandboxExecutor struct {
	goos string
}

func (e unavailableSandboxExecutor) Build(context.Context, Request) (*exec.Cmd, error) {
	return nil, fmt.Errorf("subprocess sandbox is unavailable on %s", e.goos)
}

func (e unavailableSandboxExecutor) Run(context.Context, Request) (Result, error) {
	return Result{ExitCode: -1}, fmt.Errorf("subprocess sandbox is unavailable on %s", e.goos)
}

// procfsSupport caches whether bwrap can mount a fresh procfs inside the
// sandbox namespaces on this host. Container runtimes usually mask parts of
// /proc, which makes the kernel reject new procfs mounts in a user namespace
// (EPERM); the mount plan falls back to masking /proc in that case.
var procfsSupport = struct {
	mu     sync.Mutex
	probed bool
	usable bool
	probe  func() bool
}{probe: probeProcfsMount}

func procfsMountUsable() bool {
	procfsSupport.mu.Lock()
	defer procfsSupport.mu.Unlock()
	if !procfsSupport.probed {
		procfsSupport.usable = procfsSupport.probe()
		procfsSupport.probed = true
	}
	return procfsSupport.usable
}

// overrideProcfsMountUsable pins the probe result and returns a restore
// function. Test-only.
func overrideProcfsMountUsable(usable bool) (restore func()) {
	procfsSupport.mu.Lock()
	defer procfsSupport.mu.Unlock()
	prevProbed, prevUsable := procfsSupport.probed, procfsSupport.usable
	procfsSupport.probed, procfsSupport.usable = true, usable
	return func() {
		procfsSupport.mu.Lock()
		defer procfsSupport.mu.Unlock()
		procfsSupport.probed, procfsSupport.usable = prevProbed, prevUsable
	}
}

func probeProcfsMount() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	resolved, err := exec.LookPath("bwrap")
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, resolved,
		"--unshare-user", "--unshare-pid",
		"--ro-bind", "/", "/",
		"--proc", "/proc",
		"--dev", "/dev",
		"true")
	return cmd.Run() == nil
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

// LocalExecutor is the compatibility fallback. Its child environment is the
// explicit safe environment (inherited system variables plus configured
// extras — never arbitrary parent variables), but it does not provide a
// filesystem or /proc boundary, and therefore CANNOT enforce read-only
// permission modes. It is advisory-only for non-readonly workloads; requests
// with PermissionMode == ReadOnly are rejected so that callers cannot
// silently downgrade an enforcement boundary when no real sandbox executor
// is available.
type LocalExecutor struct {
	Config Config
}

func (e LocalExecutor) Build(ctx context.Context, req Request) (*exec.Cmd, error) {
	if err := validateRequest(req); err != nil {
		return nil, err
	}
	mode := policy.NormalizePermissionMode(string(req.PermissionMode))
	if mode == policy.PermissionModeWorkspaceWrite || mode == policy.PermissionModeReadOnly {
		return nil, errors.New("LocalExecutor cannot enforce restricted permission modes; subprocess sandbox required")
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

// EnforcesFilesystem reports whether this executor can actually enforce the
// filesystem boundary on the current platform.
func (e BubblewrapExecutor) EnforcesFilesystem(policy.PermissionMode) bool {
	return runtime.GOOS == "linux"
}

// BubblewrapArgs returns the bwrap argument vector for tests and diagnostics.
func BubblewrapArgs(req Request) ([]string, error) {
	return BubblewrapArgsWithConfig(req, Config{})
}

// BubblewrapArgsWithConfig returns the bwrap argument vector using explicit
// sandbox configuration.
//
// Filesystem model: the runtime filesystem is recursively mounted read-only,
// then virtual system mounts and explicit writable overlays are applied. Writes
// are limited to /tmp, the workspace, and explicitly configured writable paths.
func BubblewrapArgsWithConfig(req Request, config Config) ([]string, error) {
	if err := validateRequest(req); err != nil {
		return nil, err
	}
	config = normalizeConfig(config)

	workDir, err := filepath.Abs(req.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("absolute workdir: %w", err)
	}
	mode := policy.NormalizePermissionMode(string(req.PermissionMode))
	readOnly := mode == policy.PermissionModeReadOnly
	if (readOnly || mode == policy.PermissionModeWorkspaceWrite) && config.WorkspaceRoot == "" {
		return nil, errors.New("restricted sandbox requires a trusted workspace root")
	}
	workspaceRoot := resolveExistingPrefix(workDir)
	if config.WorkspaceRoot != "" {
		workspaceRoot, err = workspaceRootFor(workDir, config.WorkspaceRoot)
		if err != nil {
			return nil, err
		}
	}

	args := []string{
		"--die-with-parent",
		"--unshare-user",
		"--unshare-pid",
		"--unshare-ipc",
		"--unshare-uts",
		"--unshare-cgroup-try",
	}
	if mode != policy.PermissionModeDangerFullAccess && !req.AllowNetwork {
		args = append(args, "--unshare-net")
	}
	args = append(args,
		"--uid", fmt.Sprintf("%d", os.Getuid()),
		"--gid", fmt.Sprintf("%d", os.Getgid()),
		"--clearenv",
	)
	for _, pair := range bwrapProcessEnv(req.Env, config) {
		key, val, _ := strings.Cut(pair, "=")
		args = append(args, "--setenv", key, val)
	}

	args = append(args, "--ro-bind", "/", "/")
	if procfsMountUsable() {
		// Fresh pid-namespaced /proc: host process environments are
		// unreachable and tools that need /proc/self work.
		args = append(args, "--proc", "/proc")
	} else {
		// Container runtimes usually mask parts of /proc, which makes the
		// kernel reject fresh procfs mounts inside a user namespace. Mask the
		// recursively-bound host /proc entirely so /proc/<pid>/environ of the
		// agent process (which holds host secrets) stays unreachable.
		args = append(args, "--tmpfs", "/proc")
		// Do not synthesize /proc/self/exe. A static namespace-wide link cannot
		// represent process identity: every compiler or tool launched by the
		// entrypoint would observe the entrypoint as its own executable. That
		// corrupts provenance and makes self-update probes target read-only system
		// binaries. Runtimes that require procfs must use a host where the fresh
		// procfs probe succeeds rather than receiving a convincing false identity.
	}
	args = append(args,
		"--dev", "/dev",
		"--tmpfs", "/tmp",
		"--dir", "/tmp/home",
	)
	if !readOnly {
		writableConfig := config
		writableConfig.ExtraWritablePaths = append(append([]string(nil), config.ExtraWritablePaths...), req.WritablePaths...)
		for _, path := range existingPaths(sandboxWritablePaths(workspaceRoot, writableConfig)) {
			args = append(args, "--bind", path, path)
		}
	}

	if readOnly {
		args = append(args, "--ro-bind", workspaceRoot, workspaceRoot)
	} else {
		args = append(args, "--bind", workspaceRoot, workspaceRoot)
		for _, path := range sandboxProtectedWorkspacePaths(workspaceRoot) {
			args = append(args, "--ro-bind", path, path)
		}
	}
	// Apply trusted read-only mounts after writable workspace mounts and the
	// private /tmp overlay. This both enforces configured read-only subtrees and
	// re-exposes paths hidden by /tmp (such as materialized MCP launchers).
	for _, path := range sandboxReadOnlyPaths(config) {
		args = appendSandboxReadOnlyPathArgs(args, path)
	}
	for _, path := range sandboxMaskedPaths(config) {
		args = appendSandboxMaskArgs(args, path)
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

func sandboxProtectedWorkspacePaths(workspaceRoot string) []string {
	paths := []string{
		".git/config", ".git/hooks", ".codex", ".claude", ".gemini", ".agents",
	}
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		candidate := filepath.Join(workspaceRoot, path)
		if !pathExists(candidate) {
			continue
		}
		candidate = resolveExistingPrefix(candidate)
		if !isPathWithin(candidate, workspaceRoot) {
			continue
		}
		out = append(out, candidate)
	}
	return out
}

func sandboxRunSecretPaths(exposeKubernetesServiceAccount bool) []string {
	if !exposeKubernetesServiceAccount {
		return []string{
			"/var/run/secrets/kubernetes.io/serviceaccount",
			"/run/secrets",
		}
	}

	// /var/run normally resolves to /run. Preserve masking for every other
	// runtime secret while leaving only the projected Kubernetes service-account
	// directory visible to explicitly privileged subprocesses.
	var paths []string
	entries, err := os.ReadDir("/run/secrets")
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		path := filepath.Join("/run/secrets", entry.Name())
		if entry.Name() != "kubernetes.io" {
			paths = append(paths, path)
			continue
		}
		children, childErr := os.ReadDir(path)
		if childErr != nil {
			continue
		}
		for _, child := range children {
			if child.Name() != "serviceaccount" {
				paths = append(paths, filepath.Join(path, child.Name()))
			}
		}
	}
	return paths
}

func appendSandboxMaskArgs(args []string, path string) []string {
	info, err := os.Stat(path)
	if err != nil {
		return args
	}
	if info.IsDir() {
		return append(args, "--tmpfs", path)
	}
	// Bubblewrap cannot mount tmpfs over a file. Bind the sandbox's inert null
	// device over file-based runtime secrets instead.
	return append(args, "--ro-bind", "/dev/null", path)
}

func sandboxMaskedPaths(config Config) []string {
	paths := sandboxRunSecretPaths(config.ExposeKubernetesServiceAccount)
	if home, err := os.UserHomeDir(); err == nil {
		homePaths := []string{
			".aws", ".azure", ".codex", ".claude", ".config/gcloud", ".config/gh",
			".docker", ".gemini", ".agents", ".ssh",
		}
		if !config.ExposeKubernetesServiceAccount {
			homePaths = append(homePaths, ".kube")
		}
		for _, path := range homePaths {
			paths = append(paths, filepath.Join(home, path))
		}
	}

	return normalizeSandboxMaskedPaths(paths)
}

func normalizeSandboxMaskedPaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		path = cleanAbsolutePath(path)
		if path == "" || !pathExists(path) {
			continue
		}
		path = resolveExistingPrefix(path)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		covered := false
		for _, existing := range out {
			if path == existing || isPathWithin(path, existing) {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, path)
		}
	}
	return out
}

func sandboxWritablePaths(workspaceRoot string, config Config) []string {
	var paths []string
	for _, path := range config.ExtraWritablePaths {
		paths = appendSandboxWritablePath(paths, path, workspaceRoot)
	}
	return paths
}

func sandboxReadOnlyPaths(config Config) []string {
	var paths []string
	for _, path := range config.ExtraReadOnlyPaths {
		clean := cleanAbsolutePath(path)
		if clean == "" || !pathExists(clean) {
			continue
		}
		clean = resolveExistingPrefix(clean)
		if clean == string(os.PathSeparator) || clean == "/proc" || isPathWithin(clean, "/proc") ||
			clean == "/dev" || isPathWithin(clean, "/dev") || clean == "/sys" || isPathWithin(clean, "/sys") {
			continue
		}
		covered := false
		for _, existing := range paths {
			if clean == existing || isPathWithin(clean, existing) {
				covered = true
				break
			}
		}
		if !covered {
			paths = append(paths, clean)
		}
	}
	return paths
}

func appendSandboxReadOnlyPathArgs(args []string, path string) []string {
	// The root filesystem already contains ordinary host paths. Only /tmp is
	// overlaid, so create its hidden destination parent before the bind mount.
	if isPathWithin(path, "/tmp") {
		args = append(args, "--dir", filepath.Dir(path))
	}
	return append(args, "--ro-bind", path, path)
}

func appendSandboxWritablePath(paths []string, path, workspaceRoot string) []string {
	clean := cleanAbsolutePath(path)
	if clean == "" {
		return paths
	}
	// Compare and mount symlink-resolved paths so containment checks against
	// the (resolved) workspace root hold and bind sources are real targets.
	clean = resolveExistingPrefix(clean)
	if isForbiddenSandboxWritablePath(clean, workspaceRoot) {
		return paths
	}
	for _, existing := range paths {
		if clean == existing || isPathWithin(clean, existing) {
			return paths
		}
	}
	return append(paths, clean)
}

// isForbiddenSandboxWritablePath rejects only paths whose mounts the sandbox
// manages itself (/, /proc, /dev, /tmp) and paths inside the workspace root,
// which is governed by the per-request permission mode. Hosts are otherwise
// free to configure any writable root.
func isForbiddenSandboxWritablePath(path, workspaceRoot string) bool {
	switch path {
	case "", string(os.PathSeparator), "/tmp", "/proc", "/dev", "/sys":
		return true
	}
	if workspaceRoot != "" && isPathWithin(path, filepath.Clean(workspaceRoot)) {
		return true
	}
	return false
}

// sandboxPath resolves the child PATH: an explicit Config.Path replaces the
// inherited value entirely; otherwise the parent PATH is inherited with
// configured prepends/appends applied.
func sandboxPath(config Config, inherited string) string {
	if configured := strings.TrimSpace(config.Path); configured != "" {
		if path := cleanPathList(configured); path != "" {
			return path
		}
	}

	entries := splitPathList(inherited)
	if len(entries) == 0 {
		entries = append([]string(nil), fallbackPathEntries...)
	}
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
	return flattenSafeEnv(SafeEnvMapWithConfig(config), overrides)
}

// flattenSafeEnv applies overrides (expanded against the base environment
// only) and returns a sorted KEY=value slice.
func flattenSafeEnv(base map[string]string, overrides map[string]string) []string {
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

// bwrapProcessEnv is the child environment for bubblewrap runs. It starts from
// the explicit safe environment, then homes HOME and TMPDIR into the writable
// /tmp: the rest of the filesystem is read-only inside the sandbox, and
// toolchain caches (~/.cache, ~/go, ~/.npm, ~/.cargo, ...) follow HOME
// automatically.
func bwrapProcessEnv(overrides map[string]string, config Config) []string {
	base := SafeEnvMapWithConfig(config)
	base["HOME"] = "/tmp/home"
	base["TMPDIR"] = "/tmp"
	// Non-interactive process tree: git must fail fast instead of prompting.
	base["GIT_TERMINAL_PROMPT"] = "0"
	return flattenSafeEnv(base, overrides)
}

// SafeEnvMap returns the deterministic base environment for subprocesses.
func SafeEnvMap() map[string]string {
	return SafeEnvMapWithConfig(Config{})
}

// SafeEnvMapWithConfig returns the base environment for subprocesses using
// explicit sandbox configuration: the system variables inherited from the
// parent process (identity, locale, terminal, PATH) plus explicitly configured
// extra variables. Nothing else from the parent environment is passed through.
func SafeEnvMapWithConfig(config Config) map[string]string {
	config = normalizeConfig(config)
	env := make(map[string]string)
	for _, key := range inheritedSystemEnvVars {
		if value := os.Getenv(key); strings.TrimSpace(value) != "" {
			env[key] = value
		}
	}
	for _, pair := range os.Environ() {
		key, value, ok := strings.Cut(pair, "=")
		if ok && strings.HasPrefix(key, "LC_") && validEnvKey(key) {
			env[key] = value
		}
	}
	env["PATH"] = sandboxPath(config, env["PATH"])
	if goroot := strings.TrimSpace(config.GOROOT); goroot != "" && filepath.IsAbs(goroot) {
		env["GOROOT"] = filepath.Clean(goroot)
	}
	applySandboxExtraEnv(env, config)
	return env
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
