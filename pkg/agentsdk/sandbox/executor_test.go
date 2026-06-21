package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk/policy"
)

func TestSafeEnvDoesNotInheritSecrets(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://secret")
	t.Setenv("GH_PAT", "ghp_secret")
	t.Setenv("OPENAI_API_KEY", "sk-secret")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-secret")

	env := SafeEnv(map[string]string{"CUSTOM": "$PATH:$DATABASE_URL"})
	joined := strings.Join(env, "\n")
	for _, secretName := range []string{"DATABASE_URL", "GH_PAT", "OPENAI_API_KEY", "AWS_SECRET_ACCESS_KEY"} {
		if strings.Contains(joined, secretName+"=") {
			t.Fatalf("SafeEnv leaked %s in env:\n%s", secretName, joined)
		}
	}
	if strings.Contains(joined, "postgres://secret") || strings.Contains(joined, "ghp_secret") || strings.Contains(joined, "sk-secret") {
		t.Fatalf("SafeEnv expanded parent secret in env:\n%s", joined)
	}
	if !strings.Contains(joined, "CUSTOM=/opt/gratefulagents/bin:") || !strings.Contains(joined, "/usr/local/go/bin") {
		t.Fatalf("SafeEnv did not expand safe PATH override, env:\n%s", joined)
	}
}

func TestSafeEnvIncludesWorkerToolchainDefaults(t *testing.T) {
	env := SafeEnvMapWithConfig(Config{GOROOT: "/custom/go"})
	path := env["PATH"]
	for _, want := range []string{
		"/opt/gratefulagents/bin",
		"/opt/conda/bin",
		"/usr/local/go/bin",
		"/usr/local/bin",
		"/tmp/home/.local/bin",
		"/tmp/pnpm",
		"/tmp/go/bin",
		"/tmp/cargo/bin",
		"/workspace/.cache/go/bin",
	} {
		if !strings.Contains(path, want) {
			t.Fatalf("PATH = %q, missing %q", path, want)
		}
	}
	if env["GOROOT"] != "/custom/go" {
		t.Fatalf("GOROOT = %q, want /custom/go", env["GOROOT"])
	}
	if env["GOTOOLCHAIN"] != "local" {
		t.Fatalf("GOTOOLCHAIN = %q, want local", env["GOTOOLCHAIN"])
	}
	if env["GO_TELEMETRY_CHILD"] != "2" {
		t.Fatalf("GO_TELEMETRY_CHILD = %q, want 2", env["GO_TELEMETRY_CHILD"])
	}
	for key, want := range map[string]string{
		"XDG_CACHE_HOME":   "/tmp/.cache",
		"NPM_CONFIG_CACHE": "/tmp/npm-cache",
		"PNPM_HOME":        "/tmp/pnpm",
		"PIP_CACHE_DIR":    "/tmp/pip-cache",
		"CARGO_HOME":       "/tmp/cargo",
		"GRADLE_USER_HOME": "/tmp/gradle",
		"DOTNET_CLI_HOME":  "/tmp/dotnet",
		"GEM_HOME":         "/tmp/gems",
		"COMPOSER_HOME":    "/tmp/composer",
		"DENO_DIR":         "/tmp/deno",
		"BUN_INSTALL":      "/tmp/bun",
	} {
		if env[key] != want {
			t.Fatalf("%s = %q, want %q", key, env[key], want)
		}
	}
}

func TestSafeEnvSandboxConfiguration(t *testing.T) {
	env := SafeEnvMapWithConfig(Config{
		Path: "/custom/bin:relative:/usr/bin:/custom/bin",
		ExtraEnv: map[string]string{
			"JAVA_HOME": "/opt/jdk",
			"TOOL_PATH": "$PATH:/extra,with-comma",
			"BAD-NAME":  "no",
		},
	})
	if env["PATH"] != "/custom/bin:/usr/bin" {
		t.Fatalf("PATH = %q, want configured clean path", env["PATH"])
	}
	if env["JAVA_HOME"] != "/opt/jdk" {
		t.Fatalf("JAVA_HOME = %q, want /opt/jdk", env["JAVA_HOME"])
	}
	if env["TOOL_PATH"] != "/custom/bin:/usr/bin:/extra,with-comma" {
		t.Fatalf("TOOL_PATH = %q, want expansion from sandbox PATH", env["TOOL_PATH"])
	}
	if _, ok := env["BAD-NAME"]; ok {
		t.Fatalf("invalid env key should be ignored")
	}
}

func TestConfigFromEnv(t *testing.T) {
	sep := string(os.PathListSeparator)
	t.Setenv(SandboxModeEnv, "required")
	t.Setenv(SandboxPathEnv, "/custom/bin"+sep+"relative"+sep+"/usr/bin")
	t.Setenv(SandboxPathPrependEnv, "/prepend/bin"+sep+"relative")
	t.Setenv(SandboxPathAppendEnv, "/append/bin")
	t.Setenv(SandboxExtraReadOnlyPathsEnv, "/opt/tooling"+sep+"relative")
	t.Setenv(SandboxExtraWritablePathsEnv, "/tmp/scratch"+sep+"relative")
	t.Setenv(SandboxExtraEnvEnv, "JAVA_HOME=/opt/jdk\nTOOL_PATH=$PATH:/tooling\nBAD-NAME=no")

	cfg := ConfigFromEnv()
	if cfg.Mode != "required" {
		t.Fatalf("Mode = %q, want required", cfg.Mode)
	}
	if cfg.Path != "/custom/bin"+sep+"relative"+sep+"/usr/bin" {
		t.Fatalf("Path = %q, want raw configured path", cfg.Path)
	}
	if strings.Join(cfg.PathPrepend, sep) != "/prepend/bin" {
		t.Fatalf("PathPrepend = %#v, want only clean absolute entries", cfg.PathPrepend)
	}
	if strings.Join(cfg.PathAppend, sep) != "/append/bin" {
		t.Fatalf("PathAppend = %#v, want /append/bin", cfg.PathAppend)
	}
	if strings.Join(cfg.ExtraReadOnlyPaths, sep) != "/opt/tooling" {
		t.Fatalf("ExtraReadOnlyPaths = %#v, want /opt/tooling", cfg.ExtraReadOnlyPaths)
	}
	if strings.Join(cfg.ExtraWritablePaths, sep) != "/tmp/scratch" {
		t.Fatalf("ExtraWritablePaths = %#v, want /tmp/scratch", cfg.ExtraWritablePaths)
	}
	if cfg.ExtraEnv["JAVA_HOME"] != "/opt/jdk" {
		t.Fatalf("ExtraEnv[JAVA_HOME] = %q, want /opt/jdk", cfg.ExtraEnv["JAVA_HOME"])
	}
	if _, ok := cfg.ExtraEnv["BAD-NAME"]; ok {
		t.Fatalf("invalid env key should be ignored")
	}
	env := SafeEnvMapWithConfig(cfg)
	if env["PATH"] != "/custom/bin:/usr/bin" {
		t.Fatalf("PATH = %q, want configured clean path", env["PATH"])
	}
	if env["TOOL_PATH"] != "/custom/bin:/usr/bin:/tooling" {
		t.Fatalf("TOOL_PATH = %q, want expansion from configured PATH", env["TOOL_PATH"])
	}
}

func TestSafeEnvIgnoresInvalidOverrideKeys(t *testing.T) {
	env := SafeEnv(map[string]string{
		"GOOD_NAME": "ok",
		"BAD-NAME":  "no",
		"BAD=NAME":  "no",
	})
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "GOOD_NAME=ok") {
		t.Fatalf("valid override missing from env:\n%s", joined)
	}
	if strings.Contains(joined, "BAD-NAME=") || strings.Contains(joined, "BAD=NAME=") {
		t.Fatalf("invalid override key leaked into env:\n%s", joined)
	}
}

func TestSafeEnvSandboxPathPrependAndAppend(t *testing.T) {
	path := SafeEnvMapWithConfig(Config{
		PathPrepend: []string{"/custom/bin", "relative"},
		PathAppend:  []string{"/tail/bin"},
	})["PATH"]
	if !strings.HasPrefix(path, "/custom/bin:/opt/gratefulagents/bin:") {
		t.Fatalf("PATH = %q, want configured prepend before defaults", path)
	}
	if !strings.HasSuffix(path, ":/tail/bin") {
		t.Fatalf("PATH = %q, want configured append", path)
	}
}

func TestLocalExecutorUsesSafeEnv(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://secret")

	result, err := LocalExecutor{}.Run(context.Background(), Request{
		Argv:           []string{"bash", "--noprofile", "--norc", "-c", "printf '%s' \"${DATABASE_URL:-unset}\""},
		WorkDir:        t.TempDir(),
		PermissionMode: policy.PermissionModeWorkspaceWrite,
		Timeout:        time.Second,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if strings.TrimSpace(result.Output) != "unset" {
		t.Fatalf("DATABASE_URL visible to subprocess: output=%q", result.Output)
	}
}

func TestExecutorRunCapsOutputWithoutTerminatingProcess(t *testing.T) {
	workDir := t.TempDir()
	sentinel := filepath.Join(workDir, "sentinel")
	result, err := LocalExecutor{}.Run(context.Background(), Request{
		Argv:           []string{"bash", "--noprofile", "--norc", "-c", fmt.Sprintf(`i=0; while [ "$i" -lt 20000 ]; do printf 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx'; i=$((i+1)); done; echo done > %s; echo SENTINEL_DONE`, strconv.Quote(sentinel))},
		WorkDir:        workDir,
		PermissionMode: policy.PermissionModeWorkspaceWrite,
		Timeout:        5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !result.Capped {
		t.Fatalf("Run() Capped = false, want true")
	}
	if !strings.Contains(result.Output, executorTruncationNotice) {
		t.Fatalf("Run() output missing truncation notice")
	}
	if !strings.Contains(result.Output, "SENTINEL_DONE") {
		t.Fatalf("Run() output missing post-cap tail")
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("sentinel was not written after cap: %v", err)
	}
	if len(result.Output) > maxExecutorOutputBytes+len(executorTruncationNotice)+1024 {
		t.Fatalf("output length %d exceeds cap + notice", len(result.Output))
	}
}

func TestBubblewrapArgsReadOnlyWorkspace(t *testing.T) {
	config := Config{WorkspaceRoot: "/workspace"}
	args, err := BubblewrapArgsWithConfig(Request{
		Argv:           []string{"bash", "--noprofile", "--norc", "-c", "pwd"},
		WorkDir:        "/workspace/repo",
		PermissionMode: policy.PermissionModeReadOnly,
	}, config)
	if err != nil {
		t.Fatalf("BubblewrapArgs() error = %v", err)
	}
	assertArgSequence(t, args, "--ro-bind", "/workspace", "/workspace")
	assertArgAbsent(t, args, "--bind", "/workspace", "/workspace")
	assertArgAbsent(t, args, "--proc", "/proc")
	assertArgSequence(t, args, "--dir", "/proc")
	assertArgSequence(t, args, "--tmpfs", "/tmp")
	assertArgSequence(t, args, "--clearenv")
	assertArgSequence(t, args, "--setenv", "PATH", SafeEnvMapWithConfig(config)["PATH"])
	assertArgSequence(t, args, "--setenv", "GO_TELEMETRY_CHILD", "2")
	assertArgSequence(t, args, "--chdir", "/workspace/repo")
	assertArgSequence(t, args, "--", "bash", "--noprofile", "--norc", "-c", "pwd")
}

func TestBubblewrapArgsWorkspaceWriteWorkspace(t *testing.T) {
	args, err := BubblewrapArgsWithConfig(Request{
		Argv:           []string{"bash", "--noprofile", "--norc", "-c", "printf hi > file"},
		WorkDir:        "/workspace/repo",
		PermissionMode: policy.PermissionModeWorkspaceWrite,
	}, Config{WorkspaceRoot: "/workspace"})
	if err != nil {
		t.Fatalf("BubblewrapArgs() error = %v", err)
	}
	assertArgSequence(t, args, "--bind", "/workspace", "/workspace")
	assertArgAbsent(t, args, "--ro-bind", "/workspace", "/workspace")
}

func TestBubblewrapArgsIncludesConfiguredReadOnlyToolchainPaths(t *testing.T) {
	workspace := t.TempDir()
	toolchain := t.TempDir()
	workspaceToolchain := filepath.Join(workspace, "toolchain")
	config := Config{
		WorkspaceRoot: workspace,
		ExtraReadOnlyPaths: []string{
			toolchain,
			workspaceToolchain,
			"relative",
			"/opt",
			"/tmp",
			"/var/run/secrets/kubernetes.io/serviceaccount",
			"/home/worker",
		},
	}

	args, err := BubblewrapArgsWithConfig(Request{
		Argv:           []string{"bash", "--noprofile", "--norc", "-c", "true"},
		WorkDir:        filepath.Join(workspace, "repo"),
		PermissionMode: policy.PermissionModeWorkspaceWrite,
	}, config)
	if err != nil {
		t.Fatalf("BubblewrapArgs() error = %v", err)
	}
	assertArgSequence(t, args, "--ro-bind", toolchain, toolchain)
	assertArgAbsent(t, args, "--ro-bind", workspaceToolchain, workspaceToolchain)
	assertArgAbsent(t, args, "--ro-bind", "/opt", "/opt")
	assertArgAbsent(t, args, "--ro-bind", "/tmp", "/tmp")
	assertArgAbsent(t, args, "--ro-bind", "/var/run/secrets/kubernetes.io/serviceaccount", "/var/run/secrets/kubernetes.io/serviceaccount")
	assertArgAbsent(t, args, "--ro-bind", "/home/worker", "/home/worker")
}

func TestBubblewrapArgsIncludesConfiguredWritableScratchPaths(t *testing.T) {
	workspace := t.TempDir()
	scratch, err := os.MkdirTemp("/tmp", "sdk-scratch-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(scratch) })
	nestedScratch := filepath.Join(scratch, "nested")
	if err := os.MkdirAll(nestedScratch, 0o755); err != nil {
		t.Fatal(err)
	}
	workspaceScratch := filepath.Join(workspace, "scratch")
	if err := os.MkdirAll(workspaceScratch, 0o755); err != nil {
		t.Fatal(err)
	}

	args, err := BubblewrapArgsWithConfig(Request{
		Argv:           []string{"bash", "--noprofile", "--norc", "-c", "true"},
		WorkDir:        filepath.Join(workspace, "repo"),
		PermissionMode: policy.PermissionModeWorkspaceWrite,
	}, Config{
		WorkspaceRoot: workspace,
		ExtraWritablePaths: []string{
			scratch,
			nestedScratch,
			workspaceScratch,
			"relative",
			"/tmp",
			"/etc/tmp",
		},
	})
	if err != nil {
		t.Fatalf("BubblewrapArgs() error = %v", err)
	}
	assertArgSequence(t, args, "--dir", scratch)
	assertArgSequence(t, args, "--bind", scratch, scratch)
	assertArgAbsent(t, args, "--bind", nestedScratch, nestedScratch)
	assertArgAbsent(t, args, "--bind", workspaceScratch, workspaceScratch)
	assertArgAbsent(t, args, "--bind", "/tmp", "/tmp")
	assertArgAbsent(t, args, "--bind", "/etc/tmp", "/etc/tmp")
}

func TestDefaultExecutorRequiresSandboxInKubernetes(t *testing.T) {
	_, err := DefaultWithConfig(Config{RunningInKubernetes: true}).Build(context.Background(), Request{
		Argv:           []string{"true"},
		WorkDir:        t.TempDir(),
		PermissionMode: policy.PermissionModeWorkspaceWrite,
	})
	if err == nil && runtimeHasBubblewrap() {
		return
	}
	if err == nil {
		t.Fatal("Build() unexpectedly succeeded without bubblewrap")
	}
	if !strings.Contains(err.Error(), "subprocess sandbox") {
		t.Fatalf("Build() error = %v, want subprocess sandbox failure", err)
	}
}

func TestDefaultExecutorRequiresSandboxForReadOnlyAuto(t *testing.T) {
	_, err := Default().Build(context.Background(), Request{
		Argv:           []string{"true"},
		WorkDir:        t.TempDir(),
		PermissionMode: policy.PermissionModeReadOnly,
	})
	if err == nil {
		if runtimeHasBubblewrap() {
			return
		}
		t.Fatal("Build() error = nil, want subprocess sandbox failure")
	}
	if !strings.Contains(err.Error(), "subprocess sandbox") {
		t.Fatalf("Build() error = %v, want subprocess sandbox failure", err)
	}
}

func TestLocalExecutorRejectsReadOnly(t *testing.T) {
	_, err := LocalExecutor{}.Build(context.Background(), Request{
		Argv:           []string{"true"},
		WorkDir:        t.TempDir(),
		PermissionMode: policy.PermissionModeReadOnly,
	})
	if err == nil {
		t.Fatal("LocalExecutor.Build(ReadOnly) error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "read-only") && !strings.Contains(err.Error(), "subprocess sandbox") {
		t.Fatalf("LocalExecutor.Build(ReadOnly) error = %v, want refusal mentioning sandbox/read-only", err)
	}
}

func TestDefaultExecutorDisabledModeRejectsReadOnly(t *testing.T) {
	_, err := DefaultWithConfig(Config{Mode: "disabled"}).Build(context.Background(), Request{
		Argv:           []string{"true"},
		WorkDir:        t.TempDir(),
		PermissionMode: policy.PermissionModeReadOnly,
	})
	if err == nil {
		t.Fatal("disabled mode silently accepted ReadOnly request; want non-nil error")
	}
}

func TestDefaultExecutorDisabledModeCanExplicitlySkipReadOnlySandbox(t *testing.T) {
	cmd, err := DefaultWithConfig(Config{Mode: "disabled", AllowUnsafeReadOnlyLocal: true}).Build(context.Background(), Request{
		Argv:           []string{"true"},
		WorkDir:        t.TempDir(),
		PermissionMode: policy.PermissionModeReadOnly,
	})
	if err != nil {
		t.Fatalf("Build() error = %v, want explicit local compatibility executor", err)
	}
	if cmd == nil {
		t.Fatal("Build() command = nil")
	}
}

func TestDefaultExecutorAutoModeReadOnlyFallsBackToLocalWhenOptedIn(t *testing.T) {
	if subprocessSandboxAvailable() {
		t.Skip("enforcing sandbox available on this platform; the fallback path is for non-linux dev hosts")
	}
	cmd, err := DefaultWithConfig(Config{Mode: "auto", AllowUnsafeReadOnlyLocal: true}).Build(context.Background(), Request{
		Argv:           []string{"true"},
		WorkDir:        t.TempDir(),
		PermissionMode: policy.PermissionModeReadOnly,
	})
	if err != nil {
		t.Fatalf("Build() error = %v, want LocalExecutor fallback for read-only on non-linux", err)
	}
	if cmd == nil {
		t.Fatal("Build() command = nil")
	}
}

func TestDefaultExecutorAutoModeReadOnlyFailsClosedWithoutOptIn(t *testing.T) {
	if subprocessSandboxAvailable() {
		t.Skip("enforcing sandbox available on this platform; the fail-closed path is for non-linux dev hosts")
	}
	_, err := DefaultWithConfig(Config{Mode: "auto"}).Build(context.Background(), Request{
		Argv:           []string{"true"},
		WorkDir:        t.TempDir(),
		PermissionMode: policy.PermissionModeReadOnly,
	})
	if err == nil {
		t.Fatal("Build() error = nil; read-only must fail closed without AllowUnsafeReadOnlyLocal")
	}
	if !strings.Contains(err.Error(), "subprocess sandbox") {
		t.Fatalf("Build() error = %v, want subprocess sandbox failure", err)
	}
}

func TestConfigFromEnvParsesAllowUnsafeReadOnlyLocal(t *testing.T) {
	t.Setenv(SandboxAllowUnsafeReadOnlyLocalEnv, "1")
	if !ConfigFromEnv().AllowUnsafeReadOnlyLocal {
		t.Fatalf("ConfigFromEnv().AllowUnsafeReadOnlyLocal = false, want true")
	}
	t.Setenv(SandboxAllowUnsafeReadOnlyLocalEnv, "")
	if ConfigFromEnv().AllowUnsafeReadOnlyLocal {
		t.Fatalf("ConfigFromEnv().AllowUnsafeReadOnlyLocal = true with empty env, want false")
	}
}

func TestBubblewrapArgsWorkDirEscapesConfiguredRootViaDotDot(t *testing.T) {
	// /workspace/../etc cleans to /etc — escape attempt. Even though /workspace
	// is the configured root, the resolved workdir is outside it; the executor
	// must not return an arg vector that exposes /etc as the workspace root.
	args, err := BubblewrapArgsWithConfig(Request{
		Argv:           []string{"true"},
		WorkDir:        "/workspace/../etc",
		PermissionMode: policy.PermissionModeReadOnly,
	}, Config{WorkspaceRoot: "/workspace"})
	if err != nil {
		// preferred outcome: refusal
		return
	}
	for i := 0; i+2 < len(args); i++ {
		if (args[i] == "--ro-bind" || args[i] == "--bind") && args[i+1] == "/etc" && args[i+2] == "/etc" {
			t.Fatalf("workspace root resolved to /etc; escape from /workspace not detected: %v", args)
		}
	}
}

func TestWorkspaceRootForRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	args, err := BubblewrapArgsWithConfig(Request{
		Argv:           []string{"true"},
		WorkDir:        link,
		PermissionMode: policy.PermissionModeReadOnly,
	}, Config{WorkspaceRoot: root})
	if err != nil {
		return
	}
	for i := 0; i+2 < len(args); i++ {
		if (args[i] == "--ro-bind" || args[i] == "--bind") && args[i+1] == outside {
			t.Fatalf("symlink escape allowed: workspace root resolved to %q outside configured root %q", outside, root)
		}
	}
}

func runtimeHasBubblewrap() bool {
	_, err := exec.LookPath("bwrap")
	return err == nil
}

func assertArgSequence(t *testing.T, args []string, want ...string) {
	t.Helper()
	for i := 0; i <= len(args)-len(want); i++ {
		match := true
		for j := range want {
			if args[i+j] != want[j] {
				match = false
				break
			}
		}
		if match {
			return
		}
	}
	t.Fatalf("args missing sequence %q in:\n%s", strings.Join(want, " "), strings.Join(args, " "))
}

func assertArgAbsent(t *testing.T, args []string, want ...string) {
	t.Helper()
	for i := 0; i <= len(args)-len(want); i++ {
		match := true
		for j := range want {
			if args[i+j] != want[j] {
				match = false
				break
			}
		}
		if match {
			t.Fatalf("args unexpectedly contain sequence %q in:\n%s", strings.Join(want, " "), strings.Join(args, " "))
		}
	}
}
