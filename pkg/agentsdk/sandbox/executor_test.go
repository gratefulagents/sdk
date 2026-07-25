package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
	// $PATH expands from the safe environment; $DATABASE_URL expands empty.
	wantCustom := "CUSTOM=" + SafeEnvMap()["PATH"] + ":"
	if !strings.Contains(joined, wantCustom+"\n") && !strings.HasSuffix(joined, wantCustom) {
		t.Fatalf("SafeEnv did not expand safe PATH override, env:\n%s", joined)
	}
}

func TestSafeEnvInheritsSystemVarsOnly(t *testing.T) {
	t.Setenv("LC_COLLATE", "C")
	t.Setenv("MY_APP_SECRET", "hidden")

	env := SafeEnvMap()
	if env["PATH"] != cleanPathList(os.Getenv("PATH")) {
		t.Fatalf("PATH = %q, want inherited parent PATH", env["PATH"])
	}
	if env["HOME"] != os.Getenv("HOME") {
		t.Fatalf("HOME = %q, want inherited parent HOME", env["HOME"])
	}
	if env["LC_COLLATE"] != "C" {
		t.Fatalf("LC_COLLATE = %q, want inherited LC_* variable", env["LC_COLLATE"])
	}
	if _, ok := env["MY_APP_SECRET"]; ok {
		t.Fatal("non-system parent variable leaked into safe env")
	}
}

func TestSafeEnvSandboxConfiguration(t *testing.T) {
	env := SafeEnvMapWithConfig(Config{
		Path:   "/custom/bin:relative:/usr/bin:/custom/bin",
		GOROOT: "/custom/go",
		ExtraEnv: map[string]string{
			"JAVA_HOME": "/opt/jdk",
			"TOOL_PATH": "$PATH:/extra,with-comma",
			"BAD-NAME":  "no",
		},
	})
	if env["PATH"] != "/custom/bin:/usr/bin" {
		t.Fatalf("PATH = %q, want configured clean path", env["PATH"])
	}
	if env["GOROOT"] != "/custom/go" {
		t.Fatalf("GOROOT = %q, want /custom/go", env["GOROOT"])
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
	t.Setenv(SandboxExposeKubernetesServiceAccountEnv, "true")

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
	if !cfg.ExposeKubernetesServiceAccount {
		t.Fatal("ExposeKubernetesServiceAccount = false, want true")
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
	if !strings.HasPrefix(path, "/custom/bin:") {
		t.Fatalf("PATH = %q, want configured prepend before inherited entries", path)
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
		PermissionMode: policy.PermissionModeDangerFullAccess,
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
		PermissionMode: policy.PermissionModeDangerFullAccess,
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
	restore := overrideProcfsMountUsable(true)
	defer restore()

	config := Config{WorkspaceRoot: "/workspace"}
	args, err := BubblewrapArgsWithConfig(Request{
		Argv:           []string{"bash", "--noprofile", "--norc", "-c", "pwd"},
		WorkDir:        "/workspace/repo",
		PermissionMode: policy.PermissionModeReadOnly,
	}, config)
	if err != nil {
		t.Fatalf("BubblewrapArgs() error = %v", err)
	}
	assertArgSequence(t, args, "--ro-bind", "/", "/")
	assertArgSequence(t, args, "--unshare-net")
	assertArgSequence(t, args, "--proc", "/proc")
	assertArgSequenceAfter(t, args, []string{"--ro-bind", "/", "/"}, []string{"--proc", "/proc"})
	assertArgAbsent(t, args, "--tmpfs", "/proc")
	assertArgSequence(t, args, "--dev", "/dev")
	assertArgSequence(t, args, "--tmpfs", "/tmp")
	assertArgAbsent(t, args, "--bind", "/tmp", "/tmp")
	assertArgSequence(t, args, "--dir", "/tmp/home")
	assertArgSequence(t, args, "--ro-bind", "/workspace", "/workspace")
	assertArgAbsent(t, args, "--bind", "/workspace", "/workspace")
	assertArgSequence(t, args, "--clearenv")
	assertArgSequence(t, args, "--setenv", "HOME", "/tmp/home")
	assertArgSequence(t, args, "--setenv", "TMPDIR", "/tmp")
	assertArgSequence(t, args, "--setenv", "GIT_TERMINAL_PROMPT", "0")
	assertArgSequence(t, args, "--chdir", "/workspace/repo")
	assertArgSequence(t, args, "--", "bash", "--noprofile", "--norc", "-c", "pwd")
}

func TestBubblewrapArgsDangerFullAccessKeepsHostNetwork(t *testing.T) {
	workDir := t.TempDir()
	args, err := BubblewrapArgsWithConfig(Request{
		Argv:           []string{"true"},
		WorkDir:        workDir,
		PermissionMode: policy.PermissionModeDangerFullAccess,
	}, Config{WorkspaceRoot: workDir})
	if err != nil {
		t.Fatalf("BubblewrapArgs() error = %v", err)
	}
	assertArgAbsent(t, args, "--unshare-net")
}

func TestBubblewrapArgsExplicitNetworkOptInKeepsHostNetwork(t *testing.T) {
	args, err := BubblewrapArgsWithConfig(Request{
		Argv:           []string{"true"},
		WorkDir:        "/workspace/repo",
		PermissionMode: policy.PermissionModeWorkspaceWrite,
		AllowNetwork:   true,
	}, Config{WorkspaceRoot: "/workspace"})
	if err != nil {
		t.Fatalf("BubblewrapArgs() error = %v", err)
	}
	assertArgAbsent(t, args, "--unshare-net")
}

func TestBubblewrapArgsIncludesRequestScopedWritablePath(t *testing.T) {
	workspace := t.TempDir()
	scratch := t.TempDir()
	args, err := BubblewrapArgsWithConfig(Request{
		Argv:           []string{"true"},
		WorkDir:        workspace,
		PermissionMode: policy.PermissionModeWorkspaceWrite,
		WritablePaths:  []string{scratch},
	}, Config{WorkspaceRoot: workspace})
	if err != nil {
		t.Fatalf("BubblewrapArgs() error = %v", err)
	}
	assertArgSequence(t, args, "--bind", resolveExistingPrefix(scratch), resolveExistingPrefix(scratch))
}

func TestBubblewrapArgsReadOnlyIgnoresRequestScopedWritablePath(t *testing.T) {
	workspace := t.TempDir()
	scratch := t.TempDir()
	args, err := BubblewrapArgsWithConfig(Request{
		Argv:           []string{"true"},
		WorkDir:        workspace,
		PermissionMode: policy.PermissionModeReadOnly,
		WritablePaths:  []string{scratch},
	}, Config{WorkspaceRoot: workspace})
	if err != nil {
		t.Fatalf("BubblewrapArgs() error = %v", err)
	}
	assertArgAbsent(t, args, "--bind", resolveExistingPrefix(scratch), resolveExistingPrefix(scratch))
}

func TestBubblewrapArgsWorkspaceWriteBindsWorkspaceAfterReadOnlyRoot(t *testing.T) {
	args, err := BubblewrapArgsWithConfig(Request{
		Argv:           []string{"bash", "--noprofile", "--norc", "-c", "printf hi > file"},
		WorkDir:        "/workspace/repo",
		PermissionMode: policy.PermissionModeWorkspaceWrite,
	}, Config{WorkspaceRoot: "/workspace"})
	if err != nil {
		t.Fatalf("BubblewrapArgs() error = %v", err)
	}
	assertArgSequence(t, args, "--ro-bind", "/", "/")
	assertArgSequence(t, args, "--bind", "/workspace", "/workspace")
	assertArgAbsent(t, args, "--ro-bind", "/workspace", "/workspace")
	assertArgSequence(t, args, "--tmpfs", "/tmp")
	assertArgAbsent(t, args, "--bind", "/tmp", "/tmp")
	assertArgSequenceAfter(t, args, []string{"--ro-bind", "/", "/"}, []string{"--bind", "/workspace", "/workspace"})
}

func TestBubblewrapArgsMaskProcWhenProcfsMountUnavailable(t *testing.T) {
	restore := overrideProcfsMountUsable(false)
	defer restore()

	args, err := BubblewrapArgsWithConfig(Request{
		Argv:           []string{"/usr/bin/example-runtime", "--serve"},
		WorkDir:        "/workspace/repo",
		PermissionMode: policy.PermissionModeWorkspaceWrite,
	}, Config{WorkspaceRoot: "/workspace"})
	if err != nil {
		t.Fatalf("BubblewrapArgs() error = %v", err)
	}
	// Without a fresh procfs the host /proc must remain masked so
	// /proc/<pid>/environ of the agent process is unreachable. A minimal
	// /proc/self/exe link keeps runtimes that inspect their own executable
	// working without exposing any host process metadata.
	assertArgSequence(t, args, "--tmpfs", "/proc")
	assertArgAbsent(t, args, "--proc", "/proc")
	assertArgAbsent(t, args, "--ro-bind", "/proc", "/proc")
	assertArgSequence(t, args, "--dir", "/proc/self")
	assertArgSequence(t, args, "--symlink", "/usr/bin/example-runtime", "/proc/self/exe")
}

func TestBubblewrapArgsReadsArbitraryRuntimeToolchainPathFromReadOnlyRoot(t *testing.T) {
	runtimeToolchain := filepath.Join(t.TempDir(), "runtime", "toolchain")
	if err := os.MkdirAll(runtimeToolchain, 0o755); err != nil {
		t.Fatal(err)
	}

	args, err := BubblewrapArgsWithConfig(Request{
		Argv:           []string{"bash", "--noprofile", "--norc", "-c", "true"},
		WorkDir:        "/workspace/repo",
		PermissionMode: policy.PermissionModeWorkspaceWrite,
	}, Config{WorkspaceRoot: "/workspace"})
	if err != nil {
		t.Fatalf("BubblewrapArgs() error = %v", err)
	}

	// Runtime locations need no allowlist entry: the recursive root mount makes
	// every host path readable unless a later mount deliberately masks it.
	assertArgSequence(t, args, "--ro-bind", "/", "/")
	assertArgAbsent(t, args, "--ro-bind", runtimeToolchain, runtimeToolchain)
}

func TestBubblewrapArgsRebindsProtectedWorkspaceMetadataReadOnlyAfterWorkspaceWrite(t *testing.T) {
	workspace := t.TempDir()
	protectedPaths := []string{
		filepath.Join(workspace, ".git", "config"),
		filepath.Join(workspace, ".git", "hooks"),
		filepath.Join(workspace, ".codex"),
		filepath.Join(workspace, ".claude"),
		filepath.Join(workspace, ".gemini"),
		filepath.Join(workspace, ".agents"),
	}
	for _, path := range protectedPaths {
		if filepath.Base(path) == "config" {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("[core]\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	args, err := BubblewrapArgsWithConfig(Request{
		Argv:           []string{"true"},
		WorkDir:        workspace,
		PermissionMode: policy.PermissionModeWorkspaceWrite,
	}, Config{WorkspaceRoot: workspace})
	if err != nil {
		t.Fatalf("BubblewrapArgs() error = %v", err)
	}

	workspace = resolveExistingPrefix(workspace)
	assertArgSequence(t, args, "--bind", workspace, workspace)
	for _, path := range protectedPaths {
		path = resolveExistingPrefix(path)
		assertArgSequence(t, args, "--ro-bind", path, path)
		assertArgSequenceAfter(t, args, []string{"--bind", workspace, workspace}, []string{"--ro-bind", path, path})
	}
}

func TestBubblewrapArgsMasksSensitiveHomeCredentialDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	maskedPath := filepath.Join(home, ".ssh")
	if err := os.Mkdir(maskedPath, 0o700); err != nil {
		t.Fatal(err)
	}
	resolvedHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir() error = %v", err)
	}
	if resolvedHome != home {
		t.Skipf("UserHomeDir() = %q, cannot deterministically exercise HOME-based mask", resolvedHome)
	}

	workspace := t.TempDir()
	args, err := BubblewrapArgsWithConfig(Request{
		Argv:           []string{"true"},
		WorkDir:        workspace,
		PermissionMode: policy.PermissionModeWorkspaceWrite,
	}, Config{WorkspaceRoot: workspace})
	if err != nil {
		t.Fatalf("BubblewrapArgs() error = %v", err)
	}

	maskedPath = resolveExistingPrefix(maskedPath)
	assertArgSequence(t, args, "--tmpfs", maskedPath)
	assertArgSequenceAfter(t, args, []string{"--ro-bind", "/", "/"}, []string{"--tmpfs", maskedPath})
}

func TestBubblewrapArgsExposesKubeDirectoryOnlyWhenExplicitlyEnabled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	kubeDir := filepath.Join(home, ".kube")
	if err := os.Mkdir(kubeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	resolvedHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir() error = %v", err)
	}
	if resolvedHome != home {
		t.Skipf("UserHomeDir() = %q, cannot deterministically exercise HOME-based mask", resolvedHome)
	}

	workspace := t.TempDir()
	req := Request{Argv: []string{"true"}, WorkDir: workspace, PermissionMode: policy.PermissionModeWorkspaceWrite}
	args, err := BubblewrapArgsWithConfig(req, Config{WorkspaceRoot: workspace})
	if err != nil {
		t.Fatalf("BubblewrapArgsWithConfig(default) error = %v", err)
	}
	kubeDir = resolveExistingPrefix(kubeDir)
	assertArgSequence(t, args, "--tmpfs", kubeDir)

	args, err = BubblewrapArgsWithConfig(req, Config{WorkspaceRoot: workspace, ExposeKubernetesServiceAccount: true})
	if err != nil {
		t.Fatalf("BubblewrapArgsWithConfig(exposed) error = %v", err)
	}
	assertArgAbsent(t, args, "--tmpfs", kubeDir)
}

func TestBubblewrapArgsReadOnlyIgnoresConfiguredWritablePaths(t *testing.T) {
	workspace := t.TempDir()
	scratch := t.TempDir()
	args, err := BubblewrapArgsWithConfig(Request{
		Argv:           []string{"true"},
		WorkDir:        workspace,
		PermissionMode: policy.PermissionModeReadOnly,
	}, Config{WorkspaceRoot: workspace, ExtraWritablePaths: []string{scratch}})
	if err != nil {
		t.Fatalf("BubblewrapArgs() error = %v", err)
	}
	assertArgAbsent(t, args, "--bind", resolveExistingPrefix(scratch), resolveExistingPrefix(scratch))
	assertArgSequence(t, args, "--ro-bind", resolveExistingPrefix(workspace), resolveExistingPrefix(workspace))
}

func TestBubblewrapArgsReadOnlyRejectsWorkDirOutsideTrustedRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	_, err := BubblewrapArgsWithConfig(Request{
		Argv:           []string{"true"},
		WorkDir:        outside,
		PermissionMode: policy.PermissionModeReadOnly,
	}, Config{WorkspaceRoot: root})
	if err == nil || !strings.Contains(err.Error(), "outside configured workspace root") {
		t.Fatalf("BubblewrapArgs() error = %v, want outside-root refusal", err)
	}
}

func TestBubblewrapArgsReadOnlyRequiresTrustedRoot(t *testing.T) {
	_, err := BubblewrapArgsWithConfig(Request{
		Argv:           []string{"true"},
		WorkDir:        t.TempDir(),
		PermissionMode: policy.PermissionModeReadOnly,
	}, Config{})
	if err == nil || !strings.Contains(err.Error(), "trusted workspace root") {
		t.Fatalf("BubblewrapArgs() error = %v, want trusted workspace root refusal", err)
	}
}

func TestBubblewrapArgsWorkspaceWriteRequiresTrustedRoot(t *testing.T) {
	_, err := BubblewrapArgsWithConfig(Request{
		Argv:           []string{"true"},
		WorkDir:        t.TempDir(),
		PermissionMode: policy.PermissionModeWorkspaceWrite,
	}, Config{})
	if err == nil || !strings.Contains(err.Error(), "trusted workspace root") {
		t.Fatalf("BubblewrapArgs() error = %v, want trusted workspace root refusal", err)
	}
}

func TestBubblewrapArgsIncludesConfiguredWritableScratchPaths(t *testing.T) {
	workspace := t.TempDir()
	scratch, err := os.MkdirTemp("", "sdk-scratch-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(scratch) })
	// Binds are emitted for symlink-resolved sources (macOS /var -> /private/var).
	scratch = resolveExistingPrefix(scratch)
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
			"/etc/does-not-exist-scratch",
		},
	})
	if err != nil {
		t.Fatalf("BubblewrapArgs() error = %v", err)
	}
	assertArgSequence(t, args, "--bind", scratch, scratch)
	assertArgAbsent(t, args, "--bind", nestedScratch, nestedScratch)
	assertArgAbsent(t, args, "--bind", workspaceScratch, workspaceScratch)
	assertArgAbsent(t, args, "--bind", "/etc/does-not-exist-scratch", "/etc/does-not-exist-scratch")
}

func TestExecutorEnforcesFilesystemReporting(t *testing.T) {
	wantEnforcing := platformSandboxAvailable()

	if got := ExecutorEnforcesFilesystem(DefaultWithConfig(Config{Mode: "required"}), policy.PermissionModeWorkspaceWrite); got != wantEnforcing {
		t.Fatalf("required mode enforcement = %v, want %v", got, wantEnforcing)
	}
	if ExecutorEnforcesFilesystem(DefaultWithConfig(Config{Mode: "disabled"}), policy.PermissionModeWorkspaceWrite) {
		t.Fatal("disabled mode must not report enforcement")
	}
	if got := ExecutorEnforcesFilesystem(DefaultWithConfig(Config{RunningInKubernetes: true}), policy.PermissionModeWorkspaceWrite); got != wantEnforcing {
		t.Fatalf("auto+kubernetes enforcement = %v, want %v", got, wantEnforcing)
	}
	if got := ExecutorEnforcesFilesystem(Default(), policy.PermissionModeReadOnly); got != wantEnforcing {
		t.Fatalf("auto read-only enforcement = %v, want %v", got, wantEnforcing)
	}
	if got := ExecutorEnforcesFilesystem(Default(), policy.PermissionModeWorkspaceWrite); got != wantEnforcing {
		t.Fatalf("auto workspace-write enforcement = %v, want %v", got, wantEnforcing)
	}
	if ExecutorEnforcesFilesystem(LocalExecutor{}, policy.PermissionModeWorkspaceWrite) {
		t.Fatal("LocalExecutor must not report enforcement")
	}
	if ExecutorEnforcesFilesystem(DefaultWithConfig(Config{Mode: "invalid"}), policy.PermissionModeWorkspaceWrite) {
		t.Fatal("invalid sandbox mode must not report enforcement")
	}
}

func TestDefaultExecutorRequiredModePreservesWorkspaceBoundary(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bubblewrap argument test requires Linux")
	}
	binDir := t.TempDir()
	bwrap := filepath.Join(binDir, "bwrap")
	if err := os.WriteFile(bwrap, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	workDir := t.TempDir()
	cmd, err := DefaultWithConfig(Config{Mode: "required", WorkspaceRoot: workDir}).Build(context.Background(), Request{
		Argv:           []string{"/bin/true"},
		WorkDir:        workDir,
		PermissionMode: policy.PermissionModeWorkspaceWrite,
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if cmd.Path != bwrap {
		t.Fatalf("command path = %q, want bubblewrap %q", cmd.Path, bwrap)
	}
	assertArgSequence(t, cmd.Args, "--ro-bind", "/", "/")
	assertArgSequence(t, cmd.Args, "--bind", workDir, workDir)
}

func TestDefaultExecutorRequiresSandboxInKubernetes(t *testing.T) {
	workDir := t.TempDir()
	_, err := DefaultWithConfig(Config{RunningInKubernetes: true, WorkspaceRoot: workDir}).Build(context.Background(), Request{
		Argv:           []string{"true"},
		WorkDir:        workDir,
		PermissionMode: policy.PermissionModeWorkspaceWrite,
	})
	if err == nil {
		return
	}
	if !strings.Contains(err.Error(), "subprocess sandbox") {
		t.Fatalf("Build() error = %v, want subprocess sandbox failure", err)
	}
}

func TestDefaultExecutorRequiresSandboxForReadOnlyAuto(t *testing.T) {
	workDir := t.TempDir()
	_, err := DefaultWithConfig(Config{WorkspaceRoot: workDir}).Build(context.Background(), Request{
		Argv:           []string{"true"},
		WorkDir:        workDir,
		PermissionMode: policy.PermissionModeReadOnly,
	})
	if err == nil {
		return
	}
	if !strings.Contains(err.Error(), "subprocess sandbox") {
		t.Fatalf("Build() error = %v, want subprocess sandbox failure", err)
	}
}

func TestLocalExecutorRejectsWorkspaceWrite(t *testing.T) {
	_, err := LocalExecutor{}.Build(context.Background(), Request{
		Argv:           []string{"true"},
		WorkDir:        t.TempDir(),
		PermissionMode: policy.PermissionModeWorkspaceWrite,
	})
	if err == nil || !strings.Contains(err.Error(), "restricted") {
		t.Fatalf("LocalExecutor.Build(WorkspaceWrite) error = %v, want refusal", err)
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

func TestDefaultExecutorDisabledModeUnsafeOptInStillRejectsReadOnly(t *testing.T) {
	_, err := DefaultWithConfig(Config{Mode: "disabled", AllowUnsafeReadOnlyLocal: true}).Build(context.Background(), Request{
		Argv:           []string{"true"},
		WorkDir:        t.TempDir(),
		PermissionMode: policy.PermissionModeReadOnly,
	})
	if err == nil {
		t.Fatal("unsafe compatibility flag bypassed restricted-mode containment")
	}
}

func TestDefaultExecutorAutoModeReadOnlyFailsClosedWithoutOptIn(t *testing.T) {
	if platformSandboxAvailable() {
		t.Skip("enforcing sandbox available on this platform")
	}
	_, err := DefaultWithConfig(Config{Mode: "auto"}).Build(context.Background(), Request{
		Argv:           []string{"true"},
		WorkDir:        t.TempDir(),
		PermissionMode: policy.PermissionModeReadOnly,
	})
	if err == nil {
		t.Fatal("Build() error = nil; read-only must fail closed without an enforcing sandbox")
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

func TestBubblewrapArgsWorkspaceWriteRejectsWorkDirOutsideTrustedRoot(t *testing.T) {
	_, err := BubblewrapArgsWithConfig(Request{
		Argv:           []string{"true"},
		WorkDir:        "/workspace/../etc",
		PermissionMode: policy.PermissionModeWorkspaceWrite,
	}, Config{WorkspaceRoot: "/workspace"})
	if err == nil || !strings.Contains(err.Error(), "outside configured workspace root") {
		t.Fatalf("BubblewrapArgs() error = %v, want outside-root refusal", err)
	}
}

func TestWorkspaceRootForRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	_, err := BubblewrapArgsWithConfig(Request{
		Argv:           []string{"true"},
		WorkDir:        link,
		PermissionMode: policy.PermissionModeWorkspaceWrite,
	}, Config{WorkspaceRoot: root})
	if err == nil || !strings.Contains(err.Error(), "outside configured workspace root") {
		t.Fatalf("BubblewrapArgs() error = %v, want symlink escape refusal", err)
	}
}

func assertArgSequence(t *testing.T, args []string, want ...string) {
	t.Helper()
	for i := 0; i <= len(args)-len(want); i++ {
		if argSequenceAt(args, i, want) {
			return
		}
	}
	t.Fatalf("args missing sequence %q in:\n%s", strings.Join(want, " "), strings.Join(args, " "))
}

func argSequenceAt(args []string, index int, want []string) bool {
	if index < 0 || index+len(want) > len(args) {
		return false
	}
	for j := range want {
		if args[index+j] != want[j] {
			return false
		}
	}
	return true
}

func assertArgSequenceAfter(t *testing.T, args, first, second []string) {
	t.Helper()
	firstEnd := -1
	for i := 0; i <= len(args)-len(first); i++ {
		if argSequenceAt(args, i, first) {
			firstEnd = i + len(first)
			break
		}
	}
	if firstEnd == -1 {
		t.Fatalf("args missing first sequence %q in:\n%s", strings.Join(first, " "), strings.Join(args, " "))
	}
	for i := firstEnd; i <= len(args)-len(second); i++ {
		if argSequenceAt(args, i, second) {
			return
		}
	}
	t.Fatalf("args missing second sequence %q after %q in:\n%s", strings.Join(second, " "), strings.Join(first, " "), strings.Join(args, " "))
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
