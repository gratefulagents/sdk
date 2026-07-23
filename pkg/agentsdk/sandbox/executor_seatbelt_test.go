package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk/policy"
)

func TestSeatbeltReadOnlyProfileGrantsOnlyPrivateTempWrites(t *testing.T) {
	workspace := t.TempDir()
	tempRoot := t.TempDir()

	profile, definitions, err := seatbeltProfileWithConfig(Request{
		Argv:           []string{"/bin/true"},
		WorkDir:        workspace,
		PermissionMode: policy.PermissionModeReadOnly,
	}, Config{WorkspaceRoot: workspace}, tempRoot)
	if err != nil {
		t.Fatalf("seatbeltProfileWithConfig() error = %v", err)
	}
	if !strings.Contains(profile, "(deny default)") || !strings.Contains(profile, "(allow file-read*") {
		t.Fatalf("profile lacks default-deny/global-read policy:\n%s", profile)
	}
	if strings.Contains(profile, seatbeltNetworkPolicy) || strings.Contains(profile, "(allow network*)") {
		t.Fatalf("read-only profile unexpectedly allows network:\n%s", profile)
	}
	assertDefinition(t, definitions, "WRITABLE_ROOT_0", resolveExistingPrefix(tempRoot))
	assertDefinitionValue(t, definitions, resolveExistingPrefix(os.TempDir()))
	assertDefinitionValue(t, definitions, resolveExistingPrefix(workspace))
	for _, definition := range definitions {
		if strings.HasPrefix(definition, "WRITABLE_ROOT_") && strings.HasSuffix(definition, "="+resolveExistingPrefix(workspace)) {
			t.Fatalf("read-only profile makes workspace writable: %v", definitions)
		}
	}
}

func TestSeatbeltWorkspaceWriteProfileUsesParameterizedRootsAndProtections(t *testing.T) {
	workspace := t.TempDir()
	protected := filepath.Join(workspace, ".git", "hooks")
	if err := os.MkdirAll(protected, 0o755); err != nil {
		t.Fatal(err)
	}
	extra := t.TempDir()
	requestExtra := t.TempDir()
	tempRoot := t.TempDir()

	profile, definitions, err := seatbeltProfileWithConfig(Request{
		Argv:           []string{"/bin/true"},
		WorkDir:        workspace,
		PermissionMode: policy.PermissionModeWorkspaceWrite,
		WritablePaths:  []string{requestExtra},
		AllowNetwork:   true,
	}, Config{
		WorkspaceRoot:      workspace,
		ExtraWritablePaths: []string{extra},
	}, tempRoot)
	if err != nil {
		t.Fatalf("seatbeltProfileWithConfig() error = %v", err)
	}
	if strings.Contains(profile, workspace) || strings.Contains(profile, extra) {
		t.Fatalf("profile interpolates a writable path instead of using parameters:\n%s", profile)
	}
	if !strings.Contains(profile, `(subpath (param "WRITABLE_ROOT_`) {
		t.Fatalf("profile lacks parameterized writable-root rule:\n%s", profile)
	}
	if !strings.Contains(profile, `(require-not (literal (param "WRITABLE_ROOT_`) ||
		!strings.Contains(profile, `(require-not (subpath (param "WRITABLE_ROOT_`) {
		t.Fatalf("profile lacks literal/subpath carve-outs for protected metadata:\n%s", profile)
	}
	if !strings.Contains(profile, "(allow network*)") {
		t.Fatalf("AllowNetwork profile lacks network overlay:\n%s", profile)
	}
	for _, want := range []string{tempRoot, workspace, extra, requestExtra, protected, filepath.Join(workspace, ".codex")} {
		assertDefinitionValue(t, definitions, resolveExistingPrefix(want))
	}
}

func TestSeatbeltRootSpecificRulesPreserveCredentialMasks(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("home directory unavailable: %v", err)
	}
	profile, definitions, err := seatbeltProfileWithConfig(Request{
		Argv:           []string{"/bin/true"},
		WorkDir:        home,
		PermissionMode: policy.PermissionModeWorkspaceWrite,
	}, Config{WorkspaceRoot: home}, t.TempDir())
	if err != nil {
		t.Fatalf("seatbeltProfileWithConfig() error = %v", err)
	}
	sshPath := filepath.Join(home, ".ssh")
	maskKey := ""
	for _, definition := range definitions {
		if strings.HasSuffix(definition, "="+sshPath) && strings.HasPrefix(definition, "MASKED_READ_") {
			maskKey = strings.SplitN(definition, "=", 2)[0]
			break
		}
	}
	if maskKey == "" {
		t.Fatalf("SSH credential mask missing from definitions: %v", definitions)
	}
	if count := strings.Count(profile, `(param "`+maskKey+`")`); count < 6 {
		t.Fatalf("credential mask must apply to global, readable, and writable grants; count=%d profile:\n%s", count, profile)
	}
}

func TestSeatbeltAncestorWriteRootCannotBypassWorkspaceProtections(t *testing.T) {
	ancestor := t.TempDir()
	workspace := filepath.Join(ancestor, "workspace")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	protected := filepath.Join(workspace, ".codex")
	_, definitions, err := seatbeltProfileWithConfig(Request{
		Argv:           []string{"/bin/true"},
		WorkDir:        workspace,
		PermissionMode: policy.PermissionModeWorkspaceWrite,
	}, Config{
		WorkspaceRoot:      workspace,
		ExtraWritablePaths: []string{ancestor},
	}, t.TempDir())
	if err != nil {
		t.Fatalf("seatbeltProfileWithConfig() error = %v", err)
	}
	count := 0
	for _, definition := range definitions {
		if strings.HasSuffix(definition, "="+protected) && strings.Contains(definition, "_PROTECTED_") {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("protected path must carve out both workspace and ancestor grants; count=%d definitions=%v", count, definitions)
	}
}

func TestSeatbeltSymlinkTargetProtectionAppliesToSeparateWritableRoot(t *testing.T) {
	workspace := t.TempDir()
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(workspace, ".git")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	_, definitions, err := seatbeltProfileWithConfig(Request{
		Argv:           []string{"/bin/true"},
		WorkDir:        workspace,
		PermissionMode: policy.PermissionModeWorkspaceWrite,
		WritablePaths:  []string{target},
	}, Config{WorkspaceRoot: workspace}, t.TempDir())
	if err != nil {
		t.Fatalf("seatbeltProfileWithConfig() error = %v", err)
	}
	want := filepath.Join(resolveExistingPrefix(target), "hooks")
	found := false
	for _, definition := range definitions {
		if strings.HasSuffix(definition, "="+want) && strings.Contains(definition, "_PROTECTED_") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("symlink target protection %q missing from %v", want, definitions)
	}
}

func TestSeatbeltProfileRejectsWorkDirOutsideTrustedRoot(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	_, _, err := seatbeltProfileWithConfig(Request{
		Argv:           []string{"/bin/true"},
		WorkDir:        outside,
		PermissionMode: policy.PermissionModeWorkspaceWrite,
	}, Config{WorkspaceRoot: workspace}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "outside configured workspace root") {
		t.Fatalf("seatbeltProfileWithConfig() error = %v, want outside-root refusal", err)
	}
}

func TestSeatbeltArgsKeepCommandAndPathsOutOfProfile(t *testing.T) {
	workspace := t.TempDir()
	tempRoot := t.TempDir()
	args, err := seatbeltArgsWithConfig(Request{
		Argv:           []string{"/bin/echo", "argument with spaces"},
		WorkDir:        workspace,
		PermissionMode: policy.PermissionModeWorkspaceWrite,
	}, Config{WorkspaceRoot: workspace}, tempRoot)
	if err != nil {
		t.Fatalf("seatbeltArgsWithConfig() error = %v", err)
	}
	if len(args) < 5 || args[0] != "-p" {
		t.Fatalf("unexpected Seatbelt args: %v", args)
	}
	separator := -1
	for i, arg := range args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || strings.Join(args[separator+1:], "\x00") != "/bin/echo\x00argument with spaces" {
		t.Fatalf("command argv was not preserved: %v", args)
	}
	if strings.Contains(args[1], workspace) || strings.Contains(args[1], tempRoot) {
		t.Fatalf("profile contains an unescaped host path: %s", args[1])
	}
}

func TestSeatbeltLifecycleCommandPreservesTargetEnvAndArgv(t *testing.T) {
	command := seatbeltLifecycleCommand(
		[]string{"HOME=/private/home", "TERM=xterm-256color"},
		[]string{"/bin/echo", "argument with spaces"},
	)
	joined := strings.Join(command, "\x00")
	for _, want := range []string{
		"/usr/bin/env\x00-i\x00HOME=/private/home\x00TERM=xterm-256color",
		"/bin/sh\x00-c\x00" + seatbeltCleanupScript,
		"gratefulagents-seatbelt-cleanup\x00/bin/echo\x00argument with spaces",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("lifecycle command missing %q: %v", want, command)
		}
	}
}

func TestSeatbeltLifecycleCommandCleansTempOnExit(t *testing.T) {
	tempRoot := t.TempDir()
	command := seatbeltLifecycleCommand(
		[]string{"PATH=/usr/bin:/bin", "TMPDIR=" + tempRoot},
		[]string{"/bin/true"},
	)
	if err := exec.Command(command[0], command[1:]...).Run(); err != nil {
		t.Fatalf("lifecycle command failed: %v", err)
	}
	if _, err := os.Stat(tempRoot); !os.IsNotExist(err) {
		t.Fatalf("private temp root still exists after process exit: %s (stat error %v)", tempRoot, err)
	}
}

func TestSeatbeltProcessEnvUsesPrivateHomeAndTemp(t *testing.T) {
	tempRoot := t.TempDir()
	env := strings.Join(seatbeltProcessEnv(map[string]string{
		"HOME":   "/attacker/home",
		"TMPDIR": "/attacker/tmp",
		"CUSTOM": "$HOME/value",
	}, Config{}, tempRoot), "\n")
	if !strings.Contains(env, "HOME="+filepath.Join(tempRoot, "home")) {
		t.Fatalf("Seatbelt HOME is not private:\n%s", env)
	}
	if !strings.Contains(env, "TMPDIR="+tempRoot) {
		t.Fatalf("Seatbelt TMPDIR is not private:\n%s", env)
	}
	if strings.Contains(env, "HOME=/attacker/home") || strings.Contains(env, "TMPDIR=/attacker/tmp") {
		t.Fatalf("request overrides escaped private temp root:\n%s", env)
	}
	if !strings.Contains(env, "CUSTOM="+filepath.Join(tempRoot, "home", "value")) {
		t.Fatalf("request environment did not expand against private HOME:\n%s", env)
	}
}

func TestSeatbeltExecutorEnforcementOnDarwin(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt integration test requires macOS")
	}
	workspace := t.TempDir()
	outsideDir := t.TempDir()
	inside := filepath.Join(workspace, "inside")
	outside := filepath.Join(outsideDir, "outside")
	result, err := (SeatbeltExecutor{Config: Config{WorkspaceRoot: workspace}}).Run(context.Background(), Request{
		Argv:           []string{"/bin/sh", "-c", `printf allowed > "$1"; printf denied > "$2"`, "sh", inside, outside},
		WorkDir:        workspace,
		PermissionMode: policy.PermissionModeWorkspaceWrite,
		Timeout:        5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Seatbelt Run() error = %v", err)
	}
	if result.ExitCode == 0 {
		t.Fatalf("outside write unexpectedly succeeded: %+v", result)
	}
	if _, err := os.Stat(inside); err != nil {
		t.Fatalf("allowed workspace write failed: %v; output=%s", err, result.Output)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("outside file exists or stat failed unexpectedly: %v", err)
	}
}

func TestSeatbeltBuildCleansPrivateTempOnProcessExit(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt integration test requires macOS")
	}
	workspace := t.TempDir()
	cmd, err := (SeatbeltExecutor{Config: Config{WorkspaceRoot: workspace}}).Build(context.Background(), Request{
		Argv:           []string{"/usr/bin/true"},
		WorkDir:        workspace,
		PermissionMode: policy.PermissionModeReadOnly,
	})
	if err != nil {
		t.Fatalf("Seatbelt Build() error = %v", err)
	}
	tempRoot := ""
	for _, arg := range cmd.Args {
		if strings.HasPrefix(arg, "TMPDIR=") {
			tempRoot = strings.TrimPrefix(arg, "TMPDIR=")
			break
		}
	}
	if tempRoot == "" {
		t.Fatalf("private TMPDIR missing from command: %v", cmd.Args)
	}
	if err := cmd.Run(); err != nil {
		t.Fatalf("built Seatbelt command failed: %v", err)
	}
	if _, err := os.Stat(tempRoot); !os.IsNotExist(err) {
		t.Fatalf("private temp root still exists after process exit: %s (stat error %v)", tempRoot, err)
	}
}

func assertDefinition(t *testing.T, definitions []string, key, value string) {
	t.Helper()
	want := key + "=" + value
	for _, definition := range definitions {
		if definition == want {
			return
		}
	}
	t.Fatalf("definition %q missing from %v", want, definitions)
}

func assertDefinitionValue(t *testing.T, definitions []string, value string) {
	t.Helper()
	for _, definition := range definitions {
		if strings.HasSuffix(definition, "="+value) {
			return
		}
	}
	t.Fatalf("definition value %q missing from %v", value, definitions)
}
