package mcp

import (
	"regexp"
	"strings"
)

// credentialEnvDenylist matches env-var names that commonly carry secrets.
// Names matching any of these patterns are stripped from MCP child env
// unless explicitly allowed via ServerConfig.AllowEnv.
var credentialEnvDenylist = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^AWS_.*`),
	regexp.MustCompile(`(?i)^GH_(TOKEN|PAT)$`),
	regexp.MustCompile(`(?i)^GITHUB_TOKEN$`),
	regexp.MustCompile(`(?i)^GITHUB_PAT$`),
	regexp.MustCompile(`(?i)^OPENAI_API_KEY$`),
	regexp.MustCompile(`(?i)^ANTHROPIC_API_KEY$`),
	regexp.MustCompile(`(?i)^GOOGLE_API_KEY$`),
	regexp.MustCompile(`(?i)^GEMINI_API_KEY$`),
	regexp.MustCompile(`(?i)^AZURE_.*KEY$`),
	regexp.MustCompile(`(?i)^SLACK_.*(TOKEN|SECRET|KEY)$`),
	regexp.MustCompile(`(?i)^SLACK_TOKEN$`),
	regexp.MustCompile(`(?i)^NPM_TOKEN$`),
	regexp.MustCompile(`(?i)^NPM_.*AUTH.*$`),
	regexp.MustCompile(`(?i).*_API_KEY$`),
	regexp.MustCompile(`(?i).*_SECRET$`),
	regexp.MustCompile(`(?i).*_TOKEN$`),
	regexp.MustCompile(`(?i).*_PASSWORD$`),
	regexp.MustCompile(`(?i).*_PASSWD$`),
	regexp.MustCompile(`(?i)^PASSWORD$`),
	regexp.MustCompile(`(?i)^SECRET$`),
}

// IsCredentialEnvName reports whether the env-var name matches the credential
// denylist used to filter MCP child process environment.
func IsCredentialEnvName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	for _, re := range credentialEnvDenylist {
		if re.MatchString(name) {
			return true
		}
	}
	return false
}

// FilterCredentialEnv removes credential-bearing names from env, except those
// explicitly listed in allow. It returns the filtered map and the list of
// blocked names (sorted by caller if needed).
func FilterCredentialEnv(env map[string]string, allow []string) (map[string]string, []string) {
	allowed := make(map[string]struct{}, len(allow))
	for _, a := range allow {
		allowed[strings.TrimSpace(a)] = struct{}{}
	}
	out := make(map[string]string, len(env))
	var blocked []string
	for k, v := range env {
		if _, ok := allowed[k]; ok {
			out[k] = v
			continue
		}
		if IsCredentialEnvName(k) {
			blocked = append(blocked, k)
			continue
		}
		out[k] = v
	}
	return out, blocked
}
