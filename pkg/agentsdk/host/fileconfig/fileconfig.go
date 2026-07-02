// Package fileconfig provides file-backed SDK host configuration.
//
// The loader accepts CRD-shaped ModeTemplate YAML and plain mode specs, while
// exposing only SDK-native types to clients. Built-in "chat" and "plan" modes
// are always available and can be overridden by files of the same name.
package fileconfig

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	sdkmode "github.com/gratefulagents/sdk/pkg/agentsdk/mode"
	"gopkg.in/yaml.v3"
)

// Source loads modes, roles, and host configuration from a config directory.
//
// Directory layout:
//
//	~/.gratefulagents/
//	  modes/*.yaml
//	  agents/*.md
type Source struct {
	rootDir        string
	workDir        string
	activeModeName string
}

type Option func(*Source)

func WithActiveMode(name string) Option {
	return func(s *Source) { s.activeModeName = strings.TrimSpace(name) }
}

func New(rootDir, workDir string, opts ...Option) *Source {
	if strings.TrimSpace(rootDir) == "" {
		rootDir = DefaultRootDir()
	}
	s := &Source{
		rootDir: expandUser(strings.TrimSpace(rootDir)),
		workDir: strings.TrimSpace(workDir),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s
}

func DefaultRootDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".gratefulagents"
	}
	return filepath.Join(home, ".gratefulagents")
}

func (s *Source) RootDir() string {
	if s == nil {
		return DefaultRootDir()
	}
	return s.rootDir
}

func (s *Source) ModeDir() string {
	return filepath.Join(s.RootDir(), "modes")
}

func (s *Source) AgentDir() string {
	return filepath.Join(s.RootDir(), "agents")
}

// BuiltinModes returns the built-in mode templates shipped with the SDK.
// "chat" is the interactive default; "plan" is a read-only planning preset.
// Files in the modes directory override built-ins of the same name.
func BuiltinModes() []sdkmode.TemplateSpec {
	return []sdkmode.TemplateSpec{
		{
			Name:        "chat",
			Version:     "v1",
			DisplayName: "Chat",
			Description: "Interactive chat and coding. Default mode.",
			Category:    "direct",
		},
		{
			Name:        "plan",
			Version:     "v1",
			DisplayName: "Plan",
			Description: "Read-only planning session.",
			Category:    "direct",
			ToolAccess:  "read-only",
			Instructions: strings.Join([]string{
				"PLAN MODE — Read-Only Planning",
				"",
				"Focus on understanding the problem, exploring the code, weighing tradeoffs,",
				"and producing a concrete plan. Use read-only inspection tools freely, but do",
				"not modify files, run mutating commands, or take externally visible actions.",
				"When the plan is ready, present it and wait for the user to switch modes",
				"before implementing.",
			}, "\n"),
		},
	}
}

func (s *Source) ListModes(ctx context.Context) ([]sdkmode.TemplateSpec, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.ModeDir())
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("read modes dir %s: %w", s.ModeDir(), err)
	}
	var modes []sdkmode.TemplateSpec
	seen := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() || !isYAMLFile(entry.Name()) {
			continue
		}
		spec, err := parseModeFile(filepath.Join(s.ModeDir(), entry.Name()))
		if err != nil {
			return nil, err
		}
		modes = append(modes, spec)
		seen[strings.ToLower(spec.Name)] = true
	}
	for _, builtin := range BuiltinModes() {
		if !seen[strings.ToLower(builtin.Name)] {
			modes = append(modes, builtin)
		}
	}
	sort.Slice(modes, func(i, j int) bool { return modes[i].Name < modes[j].Name })
	return modes, nil
}

func (s *Source) GetMode(ctx context.Context, name string) (*sdkmode.TemplateSpec, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("mode name is required")
	}
	if !isSafeConfigName(name) {
		return nil, fmt.Errorf("mode name %q must be a single file name, not a path", name)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, ext := range []string{".yaml", ".yml"} {
		path := filepath.Join(s.ModeDir(), name+ext)
		if _, err := os.Stat(path); err == nil {
			spec, err := parseModeFile(path)
			if err != nil {
				return nil, err
			}
			return &spec, nil
		}
	}
	modes, err := s.ListModes(ctx)
	if err != nil {
		return nil, err
	}
	for _, mode := range modes {
		if strings.EqualFold(mode.Name, name) || strings.EqualFold(mode.DisplayName, name) {
			copy := mode
			return &copy, nil
		}
	}
	return nil, fmt.Errorf("mode %q not found in %s", name, s.ModeDir())
}

func (s *Source) PermissionMode(ctx context.Context) (agentsdk.PermissionMode, error) {
	spec, err := s.ModeSnapshot(ctx)
	if err != nil {
		return "", err
	}
	if spec != nil && normalizeToolAccess(spec.ToolAccess) == "read-only" {
		return agentsdk.PermissionModeReadOnly, nil
	}
	return agentsdk.PermissionModeWorkspaceWrite, nil
}

func (s *Source) ModeSnapshot(ctx context.Context) (*sdkmode.TemplateSpec, error) {
	if s == nil || strings.TrimSpace(s.activeModeName) == "" {
		return nil, nil
	}
	return s.GetMode(ctx, s.activeModeName)
}

func (s *Source) GuardrailRules(context.Context) ([]agentsdk.GuardrailRule, error) {
	return nil, nil
}

func (s *Source) RoleCatalog(ctx context.Context) (agentsdk.RoleCatalog, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return LoadRoleCatalog(s.AgentDir())
}

func (s *Source) MCPServers(context.Context) (map[string]agentsdk.MCPServerConfig, error) {
	return nil, nil
}

func (s *Source) ModeDirective(ctx context.Context) (string, error) {
	spec, err := s.ModeSnapshot(ctx)
	if err != nil {
		return "", err
	}
	if spec == nil {
		return "", nil
	}
	return BuildModeDirective(spec), nil
}

func (s *Source) HandoffHistory(context.Context) ([]agentsdk.RunItem, error) {
	return nil, nil
}

func LoadRoleCatalog(dir string) (agentsdk.RoleCatalog, error) {
	dir = expandUser(strings.TrimSpace(dir))
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read agents dir %s: %w", dir, err)
	}
	var catalog agentsdk.RoleCatalog
	seen := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".md") {
			continue
		}
		role, err := parseRoleFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		if seen[role.Name] {
			continue
		}
		seen[role.Name] = true
		catalog = append(catalog, role)
	}
	sort.Slice(catalog, func(i, j int) bool { return catalog[i].Name < catalog[j].Name })
	return catalog, nil
}

func BuildModeDirective(spec *sdkmode.TemplateSpec) string {
	if spec == nil {
		return ""
	}
	var parts []string
	label := firstNonEmpty(spec.DisplayName, spec.Name)
	if label != "" {
		parts = append(parts, "Mode: "+label)
	}
	if strings.TrimSpace(spec.Description) != "" {
		parts = append(parts, "Mode description: "+strings.TrimSpace(spec.Description))
	}
	if normalizeToolAccess(spec.ToolAccess) == "read-only" {
		parts = append(parts, "Tool access: read-only. Do not modify files or run mutating commands.")
	}
	if strings.TrimSpace(spec.Instructions) != "" {
		parts = append(parts, strings.TrimSpace(spec.Instructions))
	}
	return strings.Join(parts, "\n\n")
}

func parseModeFile(path string) (sdkmode.TemplateSpec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return sdkmode.TemplateSpec{}, err
	}
	var doc modeDocument
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return sdkmode.TemplateSpec{}, fmt.Errorf("%s: %w", path, err)
	}
	spec := doc.Spec
	if spec.isZero() {
		if err := yaml.Unmarshal(raw, &spec); err != nil {
			return sdkmode.TemplateSpec{}, fmt.Errorf("%s: %w", path, err)
		}
	}
	out := spec.toSDK()
	if out.Name == "" {
		out.Name = firstNonEmpty(doc.Metadata.Name, strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	}
	if out.Version == "" {
		out.Version = "v1"
	}
	return out, nil
}

func parseRoleFile(path string) (agentsdk.RoleSpec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return agentsdk.RoleSpec{}, err
	}
	text := string(raw)
	role := agentsdk.RoleSpec{
		Name: strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)),
	}
	body := text
	if strings.HasPrefix(text, "---") {
		rest := strings.TrimLeft(strings.TrimPrefix(text, "---"), "\r\n")
		if end := strings.Index(rest, "\n---"); end >= 0 {
			var fm roleFrontmatter
			if err := yaml.Unmarshal([]byte(rest[:end]), &fm); err != nil {
				return agentsdk.RoleSpec{}, fmt.Errorf("%s frontmatter: %w", path, err)
			}
			role.Name = firstNonEmpty(fm.Name, role.Name)
			role.Description = fm.Description
			role.ToolAccess = normalizeToolAccess(firstNonEmpty(fm.ToolAccess, fm.ToolAccessAlt))
			role.ModelOverride = firstNonEmpty(fm.ModelOverride, fm.Model)
			body = strings.TrimLeft(rest[end+len("\n---"):], "\r\n")
		}
	}
	role.Instructions = strings.TrimSpace(body)
	if role.Name == "" {
		return role, fmt.Errorf("%s: role name is required", path)
	}
	if role.Instructions == "" {
		return role, fmt.Errorf("%s: role %q has empty instructions", path, role.Name)
	}
	if role.ToolAccess == "" {
		role.ToolAccess = "full"
	}
	return role, nil
}

type modeDocument struct {
	Metadata metadata `yaml:"metadata"`
	Spec     modeSpec `yaml:"spec"`
}

type metadata struct {
	Name string `yaml:"name"`
}

type modeSpec struct {
	Name         string            `yaml:"name"`
	Version      string            `yaml:"version"`
	DisplayName  string            `yaml:"displayName"`
	Description  string            `yaml:"description"`
	Category     string            `yaml:"category"`
	Autonomous   bool              `yaml:"autonomous"`
	ToolAccess   string            `yaml:"toolAccess"`
	Instructions string            `yaml:"instructions"`
	ModelRouting *modelRoutingSpec `yaml:"modelRouting"`
	Constraints  *constraintsSpec  `yaml:"constraints"`
}

func (s modeSpec) isZero() bool {
	return s.Name == "" &&
		s.Version == "" &&
		s.DisplayName == "" &&
		s.Description == "" &&
		s.Category == "" &&
		!s.Autonomous &&
		s.ToolAccess == "" &&
		s.Instructions == "" &&
		s.ModelRouting == nil &&
		s.Constraints == nil
}

func (s modeSpec) toSDK() sdkmode.TemplateSpec {
	out := sdkmode.TemplateSpec{
		Name:         strings.TrimSpace(s.Name),
		Version:      strings.TrimSpace(s.Version),
		DisplayName:  strings.TrimSpace(s.DisplayName),
		Description:  strings.TrimSpace(s.Description),
		Category:     strings.TrimSpace(s.Category),
		Autonomous:   s.Autonomous,
		ToolAccess:   normalizeModeToolAccess(s.ToolAccess),
		Instructions: strings.TrimSpace(s.Instructions),
	}
	if s.ModelRouting != nil {
		out.ModelRouting = s.ModelRouting.toSDK()
	}
	if s.Constraints != nil {
		out.Constraints = s.Constraints.toSDK()
	}
	return out
}

// normalizeModeToolAccess maps mode toolAccess values onto the recognized set,
// leaving empty (inherit) untouched.
func normalizeModeToolAccess(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return normalizeToolAccess(value)
}

type modelRoutingSpec struct {
	DefaultModel   string                          `yaml:"defaultModel"`
	FallbackModels []string                        `yaml:"fallbackModels"`
	ReasoningLevel string                          `yaml:"reasoningLevel"`
	TextVerbosity  string                          `yaml:"textVerbosity"`
	RoleOverrides  map[string]roleModelRoutingSpec `yaml:"roleOverrides"`
}

func (r modelRoutingSpec) toSDK() *sdkmode.ModelRouting {
	out := &sdkmode.ModelRouting{
		DefaultModel:   strings.TrimSpace(r.DefaultModel),
		FallbackModels: cloneTrimmedStrings(r.FallbackModels),
		ReasoningLevel: strings.TrimSpace(r.ReasoningLevel),
		TextVerbosity:  strings.TrimSpace(r.TextVerbosity),
		RoleOverrides:  map[string]sdkmode.RoleModelRouting{},
	}
	for name, override := range r.RoleOverrides {
		out.RoleOverrides[name] = override.toSDK()
	}
	if len(out.RoleOverrides) == 0 {
		out.RoleOverrides = nil
	}
	return out
}

type roleModelRoutingSpec struct {
	Model          string   `yaml:"model"`
	FallbackModels []string `yaml:"fallbackModels"`
	ReasoningLevel string   `yaml:"reasoningLevel"`
	TextVerbosity  string   `yaml:"textVerbosity"`
}

func (r roleModelRoutingSpec) toSDK() sdkmode.RoleModelRouting {
	return sdkmode.RoleModelRouting{
		Model:          strings.TrimSpace(r.Model),
		FallbackModels: cloneTrimmedStrings(r.FallbackModels),
		ReasoningLevel: strings.TrimSpace(r.ReasoningLevel),
		TextVerbosity:  strings.TrimSpace(r.TextVerbosity),
	}
}

func cloneTrimmedStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

type constraintsSpec struct {
	MaxTurns               int `yaml:"maxTurns"`
	SubAgentMaxTurns       int `yaml:"subAgentMaxTurns"`
	MaxConcurrentSubAgents int `yaml:"maxConcurrentSubAgents"`
	MaxRetries             int `yaml:"maxRetries"`
	MaxRuntimeMinutes      int `yaml:"maxRuntimeMinutes"`
}

func (c constraintsSpec) toSDK() *sdkmode.Constraints {
	return &sdkmode.Constraints{
		MaxTurns:               c.MaxTurns,
		SubAgentMaxTurns:       c.SubAgentMaxTurns,
		MaxConcurrentSubAgents: c.MaxConcurrentSubAgents,
		MaxRetries:             c.MaxRetries,
		MaxRuntimeMinutes:      c.MaxRuntimeMinutes,
	}
}

type roleFrontmatter struct {
	Name          string `yaml:"name"`
	Description   string `yaml:"description"`
	ToolAccess    string `yaml:"tool_access"`
	ToolAccessAlt string `yaml:"toolAccess"`
	Model         string `yaml:"model"`
	ModelOverride string `yaml:"model_override"`
}

func isYAMLFile(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".yaml") || strings.HasSuffix(lower, ".yml")
}

func isSafeConfigName(name string) bool {
	if name == "." || name == ".." || filepath.Base(name) != name || filepath.VolumeName(name) != "" {
		return false
	}
	return !strings.ContainsAny(name, `/\`)
}

func expandUser(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return home
		}
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func normalizeToolAccess(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "read_only", "readonly", "read-only", "analysis":
		return "read-only"
	case "", "full", "execution", "write", "workspace-write":
		return "full"
	default:
		return strings.TrimSpace(value)
	}
}
