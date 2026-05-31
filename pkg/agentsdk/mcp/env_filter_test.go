package mcp

import (
	"sort"
	"testing"
)

func TestFilterCredentialEnv_BlocksKnownCredentials(t *testing.T) {
	t.Parallel()

	in := map[string]string{
		"OPENAI_API_KEY":        "sk-leak",
		"ANTHROPIC_API_KEY":     "sk-ant-leak",
		"AWS_SECRET_ACCESS_KEY": "leak",
		"AWS_ACCESS_KEY_ID":     "leak",
		"GH_TOKEN":              "leak",
		"GITHUB_TOKEN":          "leak",
		"SLACK_TOKEN":           "leak",
		"NPM_TOKEN":             "leak",
		"FOO_PASSWORD":          "leak",
		"MY_API_SECRET":         "leak",
		"BENIGN_FLAG":           "ok",
	}
	got, blocked := FilterCredentialEnv(in, nil)

	if _, ok := got["BENIGN_FLAG"]; !ok {
		t.Fatalf("benign env was filtered; got=%v", got)
	}

	for _, k := range []string{
		"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "AWS_SECRET_ACCESS_KEY",
		"AWS_ACCESS_KEY_ID", "GH_TOKEN", "GITHUB_TOKEN", "SLACK_TOKEN",
		"NPM_TOKEN", "FOO_PASSWORD", "MY_API_SECRET",
	} {
		if _, ok := got[k]; ok {
			t.Errorf("expected %q to be filtered; got=%v", k, got)
		}
	}

	sort.Strings(blocked)
	if len(blocked) != 10 {
		t.Fatalf("expected 10 blocked entries, got %d (%v)", len(blocked), blocked)
	}
}

func TestFilterCredentialEnv_AllowOptIn(t *testing.T) {
	t.Parallel()

	in := map[string]string{"GH_TOKEN": "explicit"}
	got, blocked := FilterCredentialEnv(in, []string{"GH_TOKEN"})
	if got["GH_TOKEN"] != "explicit" {
		t.Fatalf("expected opt-in to retain GH_TOKEN; got=%v", got)
	}
	if len(blocked) != 0 {
		t.Fatalf("expected no blocked entries with opt-in; got=%v", blocked)
	}
}
