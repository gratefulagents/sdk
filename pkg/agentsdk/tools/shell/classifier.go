package shell

import (
	"fmt"
	"strings"

	"github.com/gratefulagents/sdk/pkg/agentsdk/policy"
)

// command represents one program invocation inside a pipeline (no |/;/&& yet).
type command struct {
	argv      []string
	hasCmdSub bool
	cmdSubs   []string
	redirects []redirect
}

type redirect struct {
	op     string // ">", ">>", "<", "<<", "<<<"
	target string
}

// pipeline is one or more commands joined by `|`.
type pipeline struct {
	commands []command
}

// parseStatements groups tokens into pipelines separated by ;, &&, ||, &.
func parseStatements(tokens []token) []pipeline {
	var pipelines []pipeline
	var pl pipeline
	var cmd command
	flushCmd := func() {
		if len(cmd.argv) > 0 || cmd.hasCmdSub || len(cmd.redirects) > 0 {
			pl.commands = append(pl.commands, cmd)
		}
		cmd = command{}
	}
	flushPipeline := func() {
		flushCmd()
		if len(pl.commands) > 0 {
			pipelines = append(pipelines, pl)
		}
		pl = pipeline{}
	}
	i := 0
	for i < len(tokens) {
		t := tokens[i]
		if !t.IsOperator {
			cmd.argv = append(cmd.argv, t.Value)
			if t.HasCmdSub {
				cmd.hasCmdSub = true
				cmd.cmdSubs = append(cmd.cmdSubs, t.CmdSubBodies...)
			}
			i++
			continue
		}
		switch t.Value {
		case ";", "&&", "||", "&", "\n":
			flushPipeline()
		case "|":
			flushCmd()
		case ">", ">>", "<", "<<", "<<<":
			target := ""
			if i+1 < len(tokens) && !tokens[i+1].IsOperator {
				target = tokens[i+1].Value
				i++
			}
			cmd.redirects = append(cmd.redirects, redirect{op: t.Value, target: target})
		}
		i++
	}
	flushPipeline()
	return pipelines
}

// ClassifyDestructiveCommand reports whether cmdLine is judged destructive by
// the shared denylist classifier, and why. It is the exported entry point for
// callers outside the bash tools (e.g. the built-in destructive-command
// guardrail) that need tokenizer-backed classification without the per-mode
// git policy applied by IsCommandBlockedForMode.
func ClassifyDestructiveCommand(cmdLine string) (bool, string) {
	return classifyDestructive(cmdLine)
}

// classifyDestructive returns (true, reason) if the command is judged
// destructive by any of our heuristics. It always operates on decoded tokens
// produced by the tokenizer, so quoting/escaping/$IFS bypasses are normalized
// before matching.
func classifyDestructive(cmdLine string) (bool, string) {
	tokens := tokenize(cmdLine)
	for _, pl := range parseStatements(tokens) {
		if blocked, reason := classifyPipeline(pl); blocked {
			return true, reason
		}
	}
	return false, ""
}

func classifyPipeline(pl pipeline) (bool, string) {
	for idx, cmd := range pl.commands {
		if blocked, reason := classifyCommand(cmd); blocked {
			return true, reason
		}
		// Pipe target receiving stdin for a shell interpreter (curl|sh).
		if idx > 0 && len(cmd.argv) > 0 && isShellInterpreter(cmd.argv[0]) {
			return true, fmt.Sprintf("piping into %q is not allowed", cmd.argv[0])
		}
	}
	return false, ""
}

func classifyCommand(cmd command) (bool, string) {
	for _, sub := range cmd.cmdSubs {
		if blocked, reason := classifyDestructive(sub); blocked {
			return true, "command substitution: " + reason
		}
	}
	for _, rd := range cmd.redirects {
		if isForbiddenWritePath(rd.target) && (rd.op == ">" || rd.op == ">>") {
			return true, fmt.Sprintf("redirect to protected path %q is not allowed", rd.target)
		}
	}
	if len(cmd.argv) == 0 {
		return false, ""
	}
	// Strip transparent prefixes: sudo, doas, env (when no var=val args),
	// command, exec. They do not change the destructiveness of the inner
	// invocation.
	argv := stripPrefixWrappers(cmd.argv)
	if len(argv) == 0 {
		return false, ""
	}
	cmd.argv = argv
	head := cmd.argv[0]
	base := strings.ToLower(basename(head))

	// Recurse into shells / eval invoked with -c "...".
	if base == "eval" {
		joined := strings.Join(cmd.argv[1:], " ")
		if blocked, reason := classifyDestructive(joined); blocked {
			return true, "eval: " + reason
		}
		return false, ""
	}
	if isShellInterpreter(head) {
		for i := 1; i < len(cmd.argv); i++ {
			if cmd.argv[i] == "-c" && i+1 < len(cmd.argv) {
				if blocked, reason := classifyDestructive(cmd.argv[i+1]); blocked {
					return true, fmt.Sprintf("%s -c: %s", base, reason)
				}
			}
		}
		// `bash <<<'rm -rf /'` and `bash << EOF\nrm -rf /\nEOF` feed the
		// here-doc/here-string content as commands to the spawned shell.
		for _, rd := range cmd.redirects {
			if rd.op == "<<<" || rd.op == "<<" {
				if blocked, reason := classifyDestructive(rd.target); blocked {
					return true, fmt.Sprintf("%s <<<: %s", base, reason)
				}
			}
		}
	}

	// rm with recursive flag pointing at root.
	if base == "rm" {
		if isRecursiveRootRemove(cmd.argv[1:]) {
			return true, "recursive removal of root paths is not allowed"
		}
	}

	// chmod -R / -Rf at root.
	if base == "chmod" || base == "chown" {
		if hasFlag(cmd.argv[1:], "R", "recursive") && containsRootTarget(cmd.argv[1:]) {
			return true, fmt.Sprintf("%s recursive at root is not allowed", base)
		}
	}

	// dd writing to a block device.
	if base == "dd" {
		for _, a := range cmd.argv[1:] {
			if strings.HasPrefix(a, "of=") {
				dest := strings.TrimPrefix(a, "of=")
				if strings.HasPrefix(dest, "/dev/") {
					return true, "dd to block device is not allowed"
				}
			}
		}
	}

	// mkfs / mkfs.* always destructive.
	if strings.HasPrefix(base, "mkfs") {
		return true, fmt.Sprintf("%s is not allowed", base)
	}

	// tee writing to /etc, /dev, /sys, /proc.
	if base == "tee" {
		for _, a := range cmd.argv[1:] {
			if strings.HasPrefix(a, "-") {
				continue
			}
			if isForbiddenWritePath(a) {
				return true, fmt.Sprintf("tee to protected path %q is not allowed", a)
			}
		}
	}

	// Fork bomb: `:(){ :|:& };:` — head token decodes to ":()" or ":(){".
	if strings.HasPrefix(head, ":()") {
		return true, "fork bomb pattern is not allowed"
	}

	// Interpreters with -c that touch system files.
	if base == "python" || base == "python3" || base == "perl" || base == "ruby" || base == "node" {
		for i := 1; i < len(cmd.argv); i++ {
			if cmd.argv[i] == "-c" && i+1 < len(cmd.argv) {
				body := cmd.argv[i+1]
				if mentionsForbiddenSystemFile(body) {
					return true, fmt.Sprintf("%s -c writing to system files is not allowed", base)
				}
			}
		}
	}

	return false, ""
}

func isRecursiveRootRemove(args []string) bool {
	recursive := false
	hasForce := false
	for _, a := range args {
		if a == "" || a == "--" {
			continue
		}
		if strings.HasPrefix(a, "--") {
			if a == "--recursive" {
				recursive = true
			}
			if a == "--force" {
				hasForce = true
			}
			continue
		}
		if strings.HasPrefix(a, "-") {
			for _, c := range a[1:] {
				switch c {
				case 'r', 'R':
					recursive = true
				case 'f':
					hasForce = true
				}
			}
			continue
		}
	}
	_ = hasForce
	if !recursive {
		return false
	}
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		if a == "/" || a == "/*" || strings.HasPrefix(a, "/*") {
			return true
		}
		// /, /etc, /usr, /home, ... all dangerous when recursive.
		if isProtectedRootDir(a) {
			return true
		}
	}
	return false
}

func containsRootTarget(args []string) bool {
	for _, a := range args {
		if a == "/" || a == "/*" || strings.HasPrefix(a, "/*") {
			return true
		}
	}
	return false
}

func isProtectedRootDir(p string) bool {
	clean := strings.TrimRight(p, "/")
	if clean == "" {
		return p == "/"
	}
	switch clean {
	case "/etc", "/usr", "/bin", "/sbin", "/lib", "/lib64", "/var",
		"/boot", "/root", "/home", "/opt", "/dev", "/proc", "/sys":
		return true
	}
	return false
}

func hasFlag(args []string, short, long string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "--") {
			if a == "--"+long {
				return true
			}
			continue
		}
		if strings.HasPrefix(a, "-") && short != "" {
			if strings.ContainsRune(a[1:], rune(short[0])) {
				return true
			}
		}
	}
	return false
}

func basename(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// stripPrefixWrappers removes leading transparent wrappers (sudo, doas,
// command, exec, nice, nohup, env-with-no-assignments) so that the underlying
// program is what gets classified. Returns the remaining argv.
func stripPrefixWrappers(argv []string) []string {
	for len(argv) > 0 {
		head := basename(argv[0])
		switch head {
		case "sudo", "doas":
			// Skip flags consumed by sudo (only the obvious ones) and any
			// VAR=value args.
			i := 1
			for i < len(argv) {
				a := argv[i]
				if strings.HasPrefix(a, "-") {
					i++
					continue
				}
				if strings.Contains(a, "=") && !strings.Contains(a, "/") {
					i++
					continue
				}
				break
			}
			argv = argv[i:]
		case "command", "exec", "nice", "nohup", "ionice", "stdbuf":
			i := 1
			for i < len(argv) && strings.HasPrefix(argv[i], "-") {
				i++
			}
			argv = argv[i:]
		case "env":
			i := 1
			for i < len(argv) {
				a := argv[i]
				if strings.HasPrefix(a, "-") {
					i++
					continue
				}
				if strings.Contains(a, "=") {
					i++
					continue
				}
				break
			}
			argv = argv[i:]
		default:
			return argv
		}
	}
	return argv
}

func isShellInterpreter(arg string) bool {
	switch basename(arg) {
	case "sh", "bash", "zsh", "ksh", "dash", "ash":
		return true
	}
	return false
}

func isForbiddenWritePath(p string) bool {
	if p == "" {
		return false
	}
	// Standard safe pseudo-device files are always valid redirect/tee targets
	// (e.g. "2>/dev/null", ">/dev/stderr"). Writing to them discards data or
	// routes it to an existing stream; it is never destructive. Without this,
	// the universally-common "2>/dev/null" idiom is blocked, which forces agents
	// to rewrite/retry exploration commands and wastes tool calls.
	if isSafeDeviceFile(p) {
		return false
	}
	if strings.HasPrefix(p, "/etc/") || p == "/etc" {
		return true
	}
	if strings.HasPrefix(p, "/dev/") || p == "/dev" {
		return true
	}
	if strings.HasPrefix(p, "/sys/") || p == "/sys" {
		return true
	}
	if strings.HasPrefix(p, "/proc/") || p == "/proc" {
		return true
	}
	if strings.HasPrefix(p, "/boot/") || p == "/boot" {
		return true
	}
	return false
}

// isSafeDeviceFile reports whether p is a standard pseudo-device that is always
// safe to use as a redirect/tee target. /dev/fd/N and /dev/std* route to the
// process's own streams; /dev/null and /dev/zero discard writes.
func isSafeDeviceFile(p string) bool {
	switch p {
	case "/dev/null", "/dev/zero", "/dev/full", "/dev/tty",
		"/dev/stdin", "/dev/stdout", "/dev/stderr",
		"/dev/random", "/dev/urandom":
		return true
	}
	return strings.HasPrefix(p, "/dev/fd/")
}

func mentionsForbiddenSystemFile(s string) bool {
	for _, marker := range []string{"/etc/passwd", "/etc/shadow", "/etc/hosts", "/etc/sudoers"} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// gitInvocations returns all argv slices in cmdLine that look like a git
// invocation, with leading `git`-equivalent token at index 0. Used by
// IsPushToProtectedBranch and the read-only-mode git denylist.
func gitInvocations(cmdLine string) [][]string {
	var out [][]string
	pipelines := parseStatements(tokenize(cmdLine))
	for _, pl := range pipelines {
		for _, cmd := range pl.commands {
			argv := stripPrefixWrappers(cmd.argv)
			if len(argv) == 0 {
				continue
			}
			head := argv[0]
			if isGitBinary(head) {
				out = append(out, argv)
				continue
			}
			// Recurse into bash -c "...", sh -c '...', eval ...
			if isShellInterpreter(head) {
				for i := 1; i < len(argv); i++ {
					if argv[i] == "-c" && i+1 < len(argv) {
						out = append(out, gitInvocations(argv[i+1])...)
					}
				}
				for _, rd := range cmd.redirects {
					if rd.op == "<<<" || rd.op == "<<" {
						out = append(out, gitInvocations(rd.target)...)
					}
				}
			}
			if basename(head) == "eval" {
				out = append(out, gitInvocations(strings.Join(argv[1:], " "))...)
			}
			// Recurse into command substitutions.
			for _, sub := range cmd.cmdSubs {
				out = append(out, gitInvocations(sub)...)
			}
		}
	}
	// Synthesize: `git;push origin main` and similar patterns where a bare
	// `git` token is followed by a separate statement whose head is a known
	// git subcommand. Real bash would not interpret these as `git push`, but
	// we treat the obfuscation defensively.
	for i := 0; i+1 < len(pipelines); i++ {
		left := pipelines[i]
		right := pipelines[i+1]
		if len(left.commands) == 0 || len(right.commands) == 0 {
			continue
		}
		lc := stripPrefixWrappers(left.commands[len(left.commands)-1].argv)
		rc := stripPrefixWrappers(right.commands[0].argv)
		if len(lc) != 1 || !isGitBinary(lc[0]) {
			continue
		}
		if len(rc) == 0 || !isLikelyGitSubcommand(rc[0]) {
			continue
		}
		synth := append([]string{"git"}, rc...)
		out = append(out, synth)
	}
	return out
}

// isLikelyGitSubcommand returns true for the subset of git subcommands that
// our policies care about. Used only for obfuscation-pattern synthesis.
func isLikelyGitSubcommand(s string) bool {
	switch s {
	case "push", "commit", "reset", "remote", "merge", "rebase",
		"cherry-pick", "add", "rm", "clean", "tag", "branch", "stash",
		"fetch", "pull", "checkout", "switch", "status", "log", "diff":
		return true
	}
	return false
}

func isGitBinary(token string) bool {
	if token == "git" {
		return true
	}
	return basename(token) == "git"
}

// gitSubcommand returns the subcommand name (e.g. "push") for a git argv, or
// "" if the argv has no subcommand. Skips global flags and `-c key=value` /
// `-C dir` option pairs.
func gitSubcommand(argv []string) string {
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		if a == "-c" || a == "-C" {
			i++
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		return a
	}
	return ""
}

func gitSubcommandArgs(argv []string) []string {
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		if a == "-c" || a == "-C" {
			i++
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		return argv[i+1:]
	}
	return nil
}

// isReadOnlyGitDenied returns (true, subcommand) if the git invocation should
// be blocked under read-only mode.
func isReadOnlyGitDenied(argv []string) (bool, string) {
	sub := gitSubcommand(argv)
	switch sub {
	case "push", "commit", "reset", "remote", "merge", "rebase",
		"cherry-pick", "add", "rm", "clean", "tag", "branch", "stash":
		return true, sub
	}
	return false, ""
}

// isWorkspaceWriteGitDenied returns (true, subcommand) if the git invocation
// should be blocked under workspace-write mode (currently just `git remote`).
func isWorkspaceWriteGitDenied(argv []string) (bool, string) {
	sub := gitSubcommand(argv)
	if sub == "remote" {
		return true, sub
	}
	return false, ""
}

// pushTargetsProtectedBranch returns true if a `git push` argv targets main /
// master.
func pushTargetsProtectedBranch(argv []string) bool {
	if gitSubcommand(argv) != "push" {
		return false
	}
	for _, a := range gitSubcommandArgs(argv) {
		if a == "" || strings.HasPrefix(a, "-") {
			continue
		}
		if isProtectedRef(a) {
			return true
		}
	}
	return false
}

// modeIsRestricted is a small helper for the public guard.
func modeIsRestricted(mode policy.PermissionMode) (readOnly, workspaceWrite bool) {
	switch policy.NormalizePermissionMode(string(mode)) {
	case policy.PermissionModeReadOnly:
		return true, false
	case policy.PermissionModeWorkspaceWrite:
		return false, true
	}
	return false, false
}
