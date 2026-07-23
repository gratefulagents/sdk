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
	"strings"
	"sync"

	"github.com/gratefulagents/sdk/pkg/agentsdk/policy"
)

const defaultSeatbeltBinary = "/usr/bin/sandbox-exec"

var seatbeltProbe = struct {
	once      sync.Once
	available bool
}{}

func seatbeltAvailable() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	seatbeltProbe.once.Do(func() {
		if resolved, err := exec.LookPath(defaultSeatbeltBinary); err != nil || resolved != defaultSeatbeltBinary {
			return
		}
		probeDir, err := os.MkdirTemp("", "gratefulagents-seatbelt-probe-")
		if err != nil {
			return
		}
		defer os.RemoveAll(probeDir)
		deniedPath := filepath.Join(probeDir, "must-not-exist")
		probePolicy := `(version 1)
(deny default)
(allow process-exec)
(allow file-read*)
`
		if err := exec.Command(defaultSeatbeltBinary, "-p", probePolicy, "--", "/usr/bin/true").Run(); err != nil {
			return
		}
		cmd := exec.Command(defaultSeatbeltBinary, "-p", probePolicy, "--", "/usr/bin/touch", deniedPath)
		if err := cmd.Run(); err == nil {
			return
		}
		_, statErr := os.Stat(deniedPath)
		seatbeltProbe.available = os.IsNotExist(statErr)
	})
	return seatbeltProbe.available
}

// SeatbeltExecutor runs commands under the macOS Seatbelt sandbox through
// sandbox-exec. sandbox-exec is deprecated by Apple, but remains the native
// per-process sandbox available to unsigned command-line programs.
type SeatbeltExecutor struct {
	Config Config
}

func (e SeatbeltExecutor) Build(ctx context.Context, req Request) (*exec.Cmd, error) {
	cmd, _, err := e.build(ctx, req)
	return cmd, err
}

func (e SeatbeltExecutor) build(ctx context.Context, req Request) (*exec.Cmd, string, error) {
	if err := validateRequest(req); err != nil {
		return nil, "", err
	}
	if !seatbeltAvailable() {
		return nil, "", errors.New("macOS Seatbelt subprocess sandbox is unavailable or failed its enforcement probe")
	}

	// Seatbelt cannot mount a private tmpfs like bubblewrap. Give each command a
	// private host temporary root. The profile excludes the shared temp parent
	// from global reads and then grants only this invocation's root.
	tempRoot, err := os.MkdirTemp("", "gratefulagents-seatbelt-")
	if err != nil {
		return nil, "", fmt.Errorf("create Seatbelt temporary root: %w", err)
	}
	home := filepath.Join(tempRoot, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		_ = os.RemoveAll(tempRoot)
		return nil, "", fmt.Errorf("create Seatbelt home: %w", err)
	}

	args, err := seatbeltArgsWithConfig(req, e.Config, tempRoot)
	if err != nil {
		_ = os.RemoveAll(tempRoot)
		return nil, "", err
	}
	separator := len(args) - len(req.Argv) - 1
	targetEnv := seatbeltProcessEnv(req.Env, e.Config, tempRoot)
	wrapped := append([]string(nil), args[:separator+1]...)
	wrapped = append(wrapped, "/usr/bin/env", "-i")
	wrapped = append(wrapped, targetEnv...)
	wrapped = append(wrapped, req.Argv...)

	cmd := exec.CommandContext(ctx, defaultSeatbeltBinary, wrapped...)
	cmd.Dir = req.WorkDir
	// Do not expose request-controlled environment values to sandbox-exec before
	// it applies Seatbelt. /usr/bin/env installs the target environment inside.
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	return cmd, tempRoot, nil
}

func (e SeatbeltExecutor) Run(ctx context.Context, req Request) (Result, error) {
	cmd, tempRoot, err := e.build(ctx, req)
	if err != nil {
		return Result{ExitCode: -1}, err
	}
	defer os.RemoveAll(tempRoot)
	return runBuiltCommand(ctx, cmd, req.Timeout)
}

// EnforcesFilesystem reports whether the trusted system Seatbelt backend passes
// a functional default-deny probe on this host.
func (e SeatbeltExecutor) EnforcesFilesystem(policy.PermissionMode) bool {
	return seatbeltAvailable()
}

func seatbeltArgsWithConfig(req Request, config Config, tempRoot string) ([]string, error) {
	profile, definitions, err := seatbeltProfileWithConfig(req, config, tempRoot)
	if err != nil {
		return nil, err
	}
	args := []string{"-p", profile}
	for _, definition := range definitions {
		args = append(args, "-D"+definition)
	}
	args = append(args, "--")
	args = append(args, req.Argv...)
	return args, nil
}

func seatbeltProfileWithConfig(req Request, config Config, tempRoot string) (string, []string, error) {
	if err := validateRequest(req); err != nil {
		return "", nil, err
	}
	config = normalizeConfig(config)

	workDir, err := filepath.Abs(req.WorkDir)
	if err != nil {
		return "", nil, fmt.Errorf("absolute workdir: %w", err)
	}
	mode := policy.NormalizePermissionMode(string(req.PermissionMode))
	if (mode == policy.PermissionModeReadOnly || mode == policy.PermissionModeWorkspaceWrite) && config.WorkspaceRoot == "" {
		return "", nil, errors.New("restricted sandbox requires a trusted workspace root")
	}
	workspaceRoot, err := workspaceRootFor(workDir, config.WorkspaceRoot)
	if err != nil {
		return "", nil, err
	}
	tempRoot = cleanAbsolutePath(tempRoot)
	if tempRoot == "" {
		return "", nil, errors.New("Seatbelt sandbox requires an absolute temporary root")
	}
	tempRoot = resolveExistingPrefix(tempRoot)

	var profile strings.Builder
	profile.WriteString(seatbeltBasePolicy)

	definitions := make([]string, 0)
	maskedPaths := seatbeltMaskedPaths()
	profile.WriteString("\n; read the host filesystem except credential-bearing and shared temporary paths\n")
	profile.WriteString("(allow file-read*\n  (require-all\n")
	for i, path := range maskedPaths {
		key := fmt.Sprintf("MASKED_READ_%d", i)
		definitions = append(definitions, key+"="+path)
		fmt.Fprintf(&profile, "    (require-not (literal (param %q)))\n", key)
		fmt.Fprintf(&profile, "    (require-not (subpath (param %q)))\n", key)
	}
	profile.WriteString("  ))\n")

	writableRoots := []string{tempRoot}
	if mode != policy.PermissionModeReadOnly {
		writableRoots = append(writableRoots, workspaceRoot)
		writableConfig := config
		writableConfig.ExtraWritablePaths = append(append([]string(nil), config.ExtraWritablePaths...), req.WritablePaths...)
		writableRoots = append(writableRoots, existingPaths(sandboxWritablePaths(workspaceRoot, writableConfig))...)
	}
	writableRoots = compactParentPaths(writableRoots)
	readableRoots := compactParentPaths(append([]string{workspaceRoot}, writableRoots...))
	for i, root := range readableRoots {
		key := fmt.Sprintf("READABLE_ROOT_%d", i)
		definitions = append(definitions, key+"="+root)
		fmt.Fprintf(&profile, "\n(allow file-read*\n  (require-all\n    (require-any (literal (param %q)) (subpath (param %q)))\n", key, key)
		for j, mask := range maskedPaths {
			if pathCoveredByRoot(mask, root) {
				maskKey := fmt.Sprintf("MASKED_READ_%d", j)
				fmt.Fprintf(&profile, "    (require-not (literal (param %q)))\n", maskKey)
				fmt.Fprintf(&profile, "    (require-not (subpath (param %q)))\n", maskKey)
			}
		}
		profile.WriteString("  ))\n")
	}

	protected := seatbeltProtectedWorkspacePaths(workspaceRoot)
	for i, root := range writableRoots {
		key := fmt.Sprintf("WRITABLE_ROOT_%d", i)
		definitions = append(definitions, key+"="+root)
		fmt.Fprintf(&profile, "\n(allow file-write*\n  (require-all\n    (require-any (literal (param %q)) (subpath (param %q)))\n", key, key)
		for j, mask := range maskedPaths {
			if pathCoveredByRoot(mask, root) {
				maskKey := fmt.Sprintf("MASKED_READ_%d", j)
				fmt.Fprintf(&profile, "    (require-not (literal (param %q)))\n", maskKey)
				fmt.Fprintf(&profile, "    (require-not (subpath (param %q)))\n", maskKey)
			}
		}
		for j, protection := range protected {
			if !pathCoveredByRoot(protection.path, root) {
				continue
			}
			protectedKey := fmt.Sprintf("WRITABLE_ROOT_%d_PROTECTED_%d", i, j)
			definitions = append(definitions, protectedKey+"="+protection.path)
			fmt.Fprintf(&profile, "    (require-not (literal (param %q)))\n", protectedKey)
			if protection.descendants {
				fmt.Fprintf(&profile, "    (require-not (subpath (param %q)))\n", protectedKey)
			}
		}
		profile.WriteString("  ))\n")
	}

	if req.AllowNetwork || mode == policy.PermissionModeDangerFullAccess {
		profile.WriteString(seatbeltNetworkPolicy)
	}

	sort.Strings(definitions)
	return profile.String(), definitions, nil
}

type seatbeltProtection struct {
	path        string
	descendants bool
}

func seatbeltProtectedWorkspacePaths(workspaceRoot string) []seatbeltProtection {
	entries := []seatbeltProtection{
		{path: "", descendants: false},
		{path: ".git", descendants: false},
		{path: ".git/config", descendants: true},
		{path: ".git/hooks", descendants: true},
		{path: ".codex", descendants: true},
		{path: ".claude", descendants: true},
		{path: ".gemini", descendants: true},
		{path: ".agents", descendants: true},
	}
	out := make([]seatbeltProtection, 0, len(entries)*3)
	seen := make(map[string]bool)
	appendProtection := func(path string, descendants bool) {
		path = filepath.Clean(path)
		key := fmt.Sprintf("%t:%s", descendants, path)
		if !seen[key] {
			out = append(out, seatbeltProtection{path: path, descendants: descendants})
			seen[key] = true
		}
	}
	for _, entry := range entries {
		lexical := filepath.Clean(filepath.Join(workspaceRoot, entry.path))
		appendProtection(lexical, entry.descendants)
		appendProtection(resolveExistingPrefix(lexical), entry.descendants)
		if info, err := os.Lstat(lexical); err == nil && info.Mode()&os.ModeSymlink != 0 {
			if target, err := os.Readlink(lexical); err == nil {
				if !filepath.IsAbs(target) {
					target = filepath.Join(filepath.Dir(lexical), target)
				}
				appendProtection(resolveExistingPrefix(target), entry.descendants)
			}
		}
	}
	return out
}

func seatbeltMaskedPaths() []string {
	paths := []string{
		"/var/run/secrets/kubernetes.io/serviceaccount",
		"/run/secrets",
		"/tmp",
		"/var/tmp",
		os.TempDir(),
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, path := range []string{
			".aws", ".azure", ".codex", ".claude", ".config/gcloud", ".config/gh",
			".docker", ".gemini", ".agents", ".kube", ".ssh",
		} {
			paths = append(paths, filepath.Join(home, path))
		}
	}

	out := make([]string, 0, len(paths)*2)
	seen := make(map[string]bool)
	for _, path := range paths {
		path = cleanAbsolutePath(path)
		if path == "" {
			continue
		}
		candidates := []string{path, resolveExistingPrefix(path)}
		if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
			if target, err := os.Readlink(path); err == nil {
				if !filepath.IsAbs(target) {
					target = filepath.Join(filepath.Dir(path), target)
				}
				candidates = append(candidates, resolveExistingPrefix(target))
			}
		}
		for _, candidate := range candidates {
			if !seen[candidate] {
				out = append(out, candidate)
				seen[candidate] = true
			}
		}
	}
	return out
}

func pathCoveredByRoot(path, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	return path == root || isPathWithin(path, root)
}

func compactParentPaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		path = cleanAbsolutePath(path)
		if path == "" {
			continue
		}
		path = resolveExistingPrefix(path)
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

func seatbeltProcessEnv(overrides map[string]string, config Config, tempRoot string) []string {
	baseConfig := config
	baseConfig.ExtraEnv = nil
	env := SafeEnvMapWithConfig(baseConfig)
	env["HOME"] = filepath.Join(tempRoot, "home")
	env["TMPDIR"] = tempRoot
	applySandboxExtraEnv(env, config)
	expansionEnv := make(map[string]string, len(env))
	for key, value := range env {
		expansionEnv[key] = value
	}
	overrideKeys := make([]string, 0, len(overrides))
	for key := range overrides {
		overrideKeys = append(overrideKeys, key)
	}
	sort.Strings(overrideKeys)
	for _, key := range overrideKeys {
		if validEnvKey(key) && key != "HOME" && key != "TMPDIR" {
			env[key] = ExpandSafe(overrides[key], expansionEnv)
		}
	}
	// These boundary variables cannot be replaced by request overrides.
	env["HOME"] = filepath.Join(tempRoot, "home")
	env["TMPDIR"] = tempRoot
	env["GIT_TERMINAL_PROMPT"] = "0"
	return flattenSafeEnv(env, nil)
}

// This policy is adapted from the Apache-2.0 licensed OpenAI Codex Seatbelt
// policy. It keeps common CLI runtimes working without granting broad Mach or
// IPC access; child processes inherit the Seatbelt policy.
const seatbeltBasePolicy = `(version 1)
(deny default)

(allow process-exec)
(allow process-fork)
(allow signal (target same-sandbox))
(allow process-info* (target same-sandbox))

(allow file-write-data
  (require-all
    (path "/dev/null")
    (vnode-type CHARACTER-DEVICE)))

(allow sysctl-read
  (sysctl-name "hw.activecpu")
  (sysctl-name "hw.byteorder")
  (sysctl-name "hw.cacheconfig")
  (sysctl-name "hw.cachelinesize_compat")
  (sysctl-name "hw.cpufamily")
  (sysctl-name "hw.cputype")
  (sysctl-name "hw.logicalcpu")
  (sysctl-name "hw.logicalcpu_max")
  (sysctl-name "hw.machine")
  (sysctl-name "hw.memsize")
  (sysctl-name "hw.model")
  (sysctl-name "hw.ncpu")
  (sysctl-name "hw.nperflevels")
  (sysctl-name-prefix "hw.optional.arm.")
  (sysctl-name-prefix "hw.optional.armv8_")
  (sysctl-name "hw.pagesize")
  (sysctl-name "hw.physicalcpu")
  (sysctl-name "hw.physicalcpu_max")
  (sysctl-name "machdep.cpu.brand_string")
  (sysctl-name "kern.argmax")
  (sysctl-name "kern.hostname")
  (sysctl-name "kern.maxfilesperproc")
  (sysctl-name "kern.maxproc")
  (sysctl-name "kern.osproductversion")
  (sysctl-name "kern.osrelease")
  (sysctl-name "kern.ostype")
  (sysctl-name "kern.osversion")
  (sysctl-name "kern.version")
  (sysctl-name "vm.loadavg")
  (sysctl-name-prefix "hw.perflevel")
  (sysctl-name-prefix "kern.proc.pgrp.")
  (sysctl-name-prefix "kern.proc.pid."))

(allow sysctl-write (sysctl-name "kern.grade_cputype"))
(allow iokit-open (iokit-registry-entry-class "RootDomainUserClient"))
(allow ipc-posix-sem)
(allow mach-lookup
  (global-name "com.apple.PowerManagement.control")
  (global-name "com.apple.system.opendirectoryd.libinfo"))

(allow pseudo-tty)
(allow file-read* file-write* file-ioctl (literal "/dev/ptmx"))
(allow file-read* file-write*
  (require-all
    (regex #"^/dev/ttys[0-9]+")
    (extension "com.apple.sandbox.pty")))
(allow file-ioctl (regex #"^/dev/ttys[0-9]+"))

(allow ipc-posix-shm-read* (ipc-posix-name-prefix "apple.cfprefs."))
(allow mach-lookup
  (global-name "com.apple.cfprefsd.daemon")
  (global-name "com.apple.cfprefsd.agent")
  (local-name "com.apple.cfprefsd.agent"))
(allow user-preference-read)
`

const seatbeltNetworkPolicy = `
; explicit host network access
(allow network*)
(allow system-socket
  (require-all
    (socket-domain AF_SYSTEM)
    (socket-protocol 2)))
(allow mach-lookup
  (global-name "com.apple.bsd.dirhelper")
  (global-name "com.apple.system.opendirectoryd.membership")
  (global-name "com.apple.SecurityServer")
  (global-name "com.apple.networkd")
  (global-name "com.apple.ocspd")
  (global-name "com.apple.trustd.agent")
  (global-name "com.apple.SystemConfiguration.DNSConfiguration")
  (global-name "com.apple.SystemConfiguration.configd"))
(allow sysctl-read (sysctl-name-regex #"^net.routetable"))
`
