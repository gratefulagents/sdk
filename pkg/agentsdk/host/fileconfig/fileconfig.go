// Package fileconfig provides file-backed SDK host configuration.
//
// The loader intentionally accepts the same CRD-shaped YAML used by the
// operator ModeTemplate and RoleInstruction fixtures, while exposing only
// SDK-native types to clients.
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
	activePhase    string
}

type Option func(*Source)

func WithActiveMode(name string) Option {
	return func(s *Source) { s.activeModeName = strings.TrimSpace(name) }
}

func WithActivePhase(phase string) Option {
	return func(s *Source) { s.activePhase = strings.TrimSpace(phase) }
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

func (s *Source) ListModes(ctx context.Context) ([]sdkmode.TemplateSpec, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.ModeDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read modes dir %s: %w", s.ModeDir(), err)
	}
	var modes []sdkmode.TemplateSpec
	for _, entry := range entries {
		if entry.IsDir() || !isYAMLFile(entry.Name()) {
			continue
		}
		spec, err := parseModeFile(filepath.Join(s.ModeDir(), entry.Name()))
		if err != nil {
			return nil, err
		}
		modes = append(modes, spec)
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
	phase := SelectPhase(spec, s.activePhase)
	if phase != nil && phase.ReadOnly {
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

func (s *Source) PhaseDirective(ctx context.Context) (string, error) {
	spec, err := s.ModeSnapshot(ctx)
	if err != nil {
		return "", err
	}
	if spec == nil {
		return "", nil
	}
	return BuildModeDirective(spec, s.activePhase), nil
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

func SelectPhase(spec *sdkmode.TemplateSpec, preferred string) *sdkmode.Phase {
	if spec == nil || len(spec.Phases) == 0 {
		return nil
	}
	preferred = strings.TrimSpace(preferred)
	if preferred != "" {
		for i := range spec.Phases {
			if spec.Phases[i].ID == preferred {
				return &spec.Phases[i]
			}
		}
	}
	return &spec.Phases[0]
}

func BuildModeDirective(spec *sdkmode.TemplateSpec, activePhase string) string {
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
	if phase := SelectPhase(spec, activePhase); phase != nil {
		line := "Active phase: " + phase.ID
		if phase.Name != "" && phase.Name != phase.ID {
			line += " (" + phase.Name + ")"
		}
		var flags []string
		if phase.ReadOnly {
			flags = append(flags, "read-only")
		}
		if phase.RequiresApproval {
			flags = append(flags, "requires approval")
		}
		if len(flags) > 0 {
			line += " [" + strings.Join(flags, ", ") + "]"
		}
		parts = append(parts, line)
		if strings.TrimSpace(phase.Description) != "" {
			parts = append(parts, "Phase description: "+strings.TrimSpace(phase.Description))
		}
		if len(phase.EntryGates) > 0 {
			var gates []string
			for _, gate := range phase.EntryGates {
				if gate.Require == "" {
					continue
				}
				msg := strings.TrimSpace(gate.Message)
				if msg != "" {
					gates = append(gates, "- "+gate.Require+": "+msg)
				} else {
					gates = append(gates, "- "+gate.Require)
				}
			}
			if len(gates) > 0 {
				parts = append(parts, "Phase entry gates:\n"+strings.Join(gates, "\n"))
			}
		}
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
	Name             string            `yaml:"name"`
	Version          string            `yaml:"version"`
	DisplayName      string            `yaml:"displayName"`
	Description      string            `yaml:"description"`
	Category         string            `yaml:"category"`
	Autonomous       bool              `yaml:"autonomous"`
	Instructions     string            `yaml:"instructions"`
	Phases           []phaseSpec       `yaml:"phases"`
	Transitions      []transitionSpec  `yaml:"transitions"`
	Capabilities     []string          `yaml:"capabilities"`
	ModelRouting     *modelRoutingSpec `yaml:"modelRouting"`
	RoleInstructions map[string]string `yaml:"roleInstructions"`
	Constraints      *constraintsSpec  `yaml:"constraints"`
	ResetTo          *resetToSpec      `yaml:"resetTo"`
}

func (s modeSpec) isZero() bool {
	return s.Name == "" &&
		s.Version == "" &&
		s.DisplayName == "" &&
		s.Description == "" &&
		s.Category == "" &&
		!s.Autonomous &&
		s.Instructions == "" &&
		len(s.Phases) == 0 &&
		s.ModelRouting == nil &&
		s.Constraints == nil &&
		s.ResetTo == nil
}

func (s modeSpec) toSDK() sdkmode.TemplateSpec {
	out := sdkmode.TemplateSpec{
		Name:             strings.TrimSpace(s.Name),
		Version:          strings.TrimSpace(s.Version),
		DisplayName:      strings.TrimSpace(s.DisplayName),
		Description:      strings.TrimSpace(s.Description),
		Category:         strings.TrimSpace(s.Category),
		Autonomous:       s.Autonomous,
		Instructions:     strings.TrimSpace(s.Instructions),
		Capabilities:     append([]string(nil), s.Capabilities...),
		RoleInstructions: cloneStringMap(s.RoleInstructions),
	}
	for _, p := range s.Phases {
		out.Phases = append(out.Phases, p.toSDK())
	}
	for _, tr := range s.Transitions {
		out.Transitions = append(out.Transitions, tr.toSDK())
	}
	if s.ModelRouting != nil {
		out.ModelRouting = s.ModelRouting.toSDK()
	}
	if s.Constraints != nil {
		out.Constraints = s.Constraints.toSDK()
	}
	if s.ResetTo != nil {
		out.ResetTo = s.ResetTo.toSDK()
	}
	return out
}

type phaseSpec struct {
	ID               string     `yaml:"id"`
	Name             string     `yaml:"name"`
	Description      string     `yaml:"description"`
	ReadOnly         bool       `yaml:"readOnly"`
	RequiresApproval bool       `yaml:"requiresApproval"`
	PresentArtifact  string     `yaml:"presentArtifact"`
	EntryGates       []gateSpec `yaml:"entryGates"`
}

func (p phaseSpec) toSDK() sdkmode.Phase {
	out := sdkmode.Phase{
		ID:               strings.TrimSpace(p.ID),
		Name:             strings.TrimSpace(p.Name),
		Description:      strings.TrimSpace(p.Description),
		ReadOnly:         p.ReadOnly,
		RequiresApproval: p.RequiresApproval,
		PresentArtifact:  strings.TrimSpace(p.PresentArtifact),
	}
	for _, gate := range p.EntryGates {
		out.EntryGates = append(out.EntryGates, gate.toSDK())
	}
	return out
}

type gateSpec struct {
	Require string `yaml:"require"`
	Message string `yaml:"message"`
}

func (g gateSpec) toSDK() sdkmode.PhaseGate {
	return sdkmode.PhaseGate{
		Require: strings.TrimSpace(g.Require),
		Message: strings.TrimSpace(g.Message),
	}
}

type transitionSpec struct {
	From  string   `yaml:"from"`
	To    string   `yaml:"to"`
	Gates []string `yaml:"gates"`
	When  []string `yaml:"when"`
}

func (t transitionSpec) toSDK() sdkmode.Transition {
	return sdkmode.Transition{
		From:  strings.TrimSpace(t.From),
		To:    strings.TrimSpace(t.To),
		Gates: append([]string(nil), t.Gates...),
		When:  append([]string(nil), t.When...),
	}
}

type modelRoutingSpec struct {
	DefaultModel   string                          `yaml:"defaultModel"`
	ReasoningLevel string                          `yaml:"reasoningLevel"`
	TextVerbosity  string                          `yaml:"textVerbosity"`
	RoleOverrides  map[string]roleModelRoutingSpec `yaml:"roleOverrides"`
}

func (r modelRoutingSpec) toSDK() *sdkmode.ModelRouting {
	out := &sdkmode.ModelRouting{
		DefaultModel:   strings.TrimSpace(r.DefaultModel),
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
	Model          string `yaml:"model"`
	ReasoningLevel string `yaml:"reasoningLevel"`
	TextVerbosity  string `yaml:"textVerbosity"`
}

func (r roleModelRoutingSpec) toSDK() sdkmode.RoleModelRouting {
	return sdkmode.RoleModelRouting{
		Model:          strings.TrimSpace(r.Model),
		ReasoningLevel: strings.TrimSpace(r.ReasoningLevel),
		TextVerbosity:  strings.TrimSpace(r.TextVerbosity),
	}
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

type resetToSpec struct {
	Mode             string `yaml:"mode"`
	Name             string `yaml:"name"`
	Version          string `yaml:"version"`
	Prompt           string `yaml:"prompt"`
	RequiresApproval bool   `yaml:"requiresApproval"`
	ClearHistory     bool   `yaml:"clearHistory"`
}

func (r resetToSpec) toSDK() *sdkmode.ResetTo {
	return &sdkmode.ResetTo{
		Name:             firstNonEmpty(r.Name, r.Mode),
		Version:          strings.TrimSpace(r.Version),
		Prompt:           strings.TrimSpace(r.Prompt),
		RequiresApproval: r.RequiresApproval,
		ClearHistory:     r.ClearHistory,
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
