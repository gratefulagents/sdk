package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	"golang.org/x/net/html"
)

const (
	defaultMaxLength = 50000
	maxRawBodySize   = 2 * 1024 * 1024
	fetchTimeout     = 15 * time.Second
	fetchUserAgent   = "gratefulagents-bot/1.0"
)

// FetchTool fetches a URL and converts HTML to readable text.
type FetchTool struct {
	AllowPrivateNetworkURLs bool
}

type fetchInput struct {
	URL        string `json:"url"`
	MaxLength  int    `json:"max_length"`
	StartIndex int    `json:"start_index"`
}

func (t *FetchTool) Name() string { return "WebFetch" }

func (t *FetchTool) Description() string {
	return "Fetches a URL and returns its content as readable text. HTML pages are converted to a simplified readable format. Supports pagination through large content via start_index."
}

func (t *FetchTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"url": {
				"type": "string",
				"description": "URL to fetch and convert to readable text"
			},
			"max_length": {
				"type": "integer",
				"description": "Maximum characters to return (default: 50000)"
			},
			"start_index": {
				"type": "integer",
				"description": "Start index for pagination through content (default: 0)"
			}
		},
		"required": ["url"]
	}`)
}

func (t *FetchTool) IsReadOnly() bool { return true }

func (t *FetchTool) IsEnabled(_ *agentsdk.RunContext) bool { return true }

func (t *FetchTool) NeedsApproval() bool { return false }

func (t *FetchTool) TimeoutSeconds() int { return 0 }

func (t *FetchTool) Execute(ctx context.Context, input json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in fetchInput
	if err := json.Unmarshal(input, &in); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Invalid input: %v", err), IsError: true}, nil
	}
	if in.URL == "" {
		return agentsdk.ToolResult{Content: "url is required", IsError: true}, nil
	}
	security := URLSecurityOptions{AllowPrivateNetworkURLs: t.AllowPrivateNetworkURLs}
	parsedURL, err := ValidateHTTPURL(ctx, in.URL, security)
	if err != nil {
		return agentsdk.ToolResult{Content: err.Error(), IsError: true}, nil
	}

	maxLen := in.MaxLength
	if maxLen <= 0 {
		maxLen = defaultMaxLength
	}

	client := newSafeHTTPClientWithOptions(fetchTimeout, security)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsedURL.String(), nil)
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Failed to create request: %v", err), IsError: true}, nil
	}
	req.Header.Set("User-Agent", fetchUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Fetch failed: %v", err), IsError: true}, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return agentsdk.ToolResult{Content: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, resp.Status), IsError: true}, nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRawBodySize))
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Failed to read response: %v", err), IsError: true}, nil
	}

	content := string(body)
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/html") || strings.Contains(ct, "application/xhtml") {
		content = HTMLToText(content)
	}

	if in.StartIndex > 0 {
		if in.StartIndex >= len(content) {
			return agentsdk.ToolResult{Content: fmt.Sprintf("start_index %d exceeds content length %d", in.StartIndex, len(content))}, nil
		}
		content = content[in.StartIndex:]
	}

	truncated := false
	if len(content) > maxLen {
		content = content[:maxLen]
		truncated = true
	}
	if truncated {
		nextIndex := in.StartIndex + maxLen
		content += fmt.Sprintf("\n\n--- Content truncated. Use start_index=%d to continue reading. ---", nextIndex)
	}
	return agentsdk.ToolResult{Content: content}, nil
}

// HTMLToText converts HTML to simplified readable text.
func HTMLToText(rawHTML string) string {
	tokenizer := html.NewTokenizer(strings.NewReader(rawHTML))

	var b strings.Builder
	skipTags := map[string]bool{"script": true, "style": true, "nav": true, "footer": true}
	skipDepth := 0
	skipTag := ""
	var anchorHrefs []string

	for {
		tt := tokenizer.Next()
		if tt == html.ErrorToken {
			break
		}
		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			tn, hasAttr := tokenizer.TagName()
			tagName := string(tn)

			if skipDepth > 0 {
				if tagName == skipTag {
					skipDepth++
				}
				continue
			}
			if skipTags[tagName] {
				skipDepth = 1
				skipTag = tagName
				continue
			}

			switch tagName {
			case "h1", "h2", "h3", "h4", "h5", "h6":
				b.WriteString("\n\n# ")
			case "p", "div", "section", "article":
				b.WriteString("\n\n")
			case "br":
				b.WriteString("\n")
			case "li":
				b.WriteString("\n- ")
			case "ul", "ol":
				b.WriteString("\n")
			case "code", "pre":
				b.WriteString("`")
			case "a":
				href := ""
				if hasAttr {
					href = getAttr(tokenizer, "href")
				}
				anchorHrefs = append(anchorHrefs, href)
				if href != "" {
					b.WriteString("[")
				}
			}

		case html.EndTagToken:
			tn, _ := tokenizer.TagName()
			tagName := string(tn)

			if skipDepth > 0 {
				if tagName == skipTag {
					skipDepth--
				}
				continue
			}

			switch tagName {
			case "h1", "h2", "h3", "h4", "h5", "h6":
				b.WriteString("\n")
			case "p", "div", "section", "article":
				b.WriteString("\n")
			case "code", "pre":
				b.WriteString("`")
			case "a":
				if len(anchorHrefs) > 0 {
					href := anchorHrefs[len(anchorHrefs)-1]
					anchorHrefs = anchorHrefs[:len(anchorHrefs)-1]
					if href != "" {
						b.WriteString("](")
						b.WriteString(href)
						b.WriteString(") ")
					}
				}
			}

		case html.TextToken:
			if skipDepth > 0 {
				continue
			}
			text := strings.TrimSpace(string(tokenizer.Text()))
			if text != "" {
				b.WriteString(text)
				b.WriteString(" ")
			}
		}
	}

	return CollapseWhitespace(b.String())
}

func getAttr(z *html.Tokenizer, name string) string {
	for {
		key, val, more := z.TagAttr()
		if string(key) == name {
			return string(val)
		}
		if !more {
			break
		}
	}
	return ""
}

// CollapseWhitespace reduces runs of whitespace and blank lines.
func CollapseWhitespace(s string) string {
	lines := strings.Split(s, "\n")
	var out []string
	blanks := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			blanks++
			if blanks <= 1 {
				out = append(out, "")
			}
		} else {
			blanks = 0
			out = append(out, trimmed)
		}
	}
	result := strings.Join(out, "\n")
	return strings.TrimSpace(result)
}
