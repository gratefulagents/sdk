package lsp

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/gratefulagents/sdk/pkg/agentsdk/tools/internal/pathutil"
)

type lspPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type lspRange struct {
	Start lspPosition `json:"start"`
	End   lspPosition `json:"end"`
}

type lspLocation struct {
	URI   string   `json:"uri"`
	Range lspRange `json:"range"`
}

type lspLocationLink struct {
	TargetURI            string   `json:"targetUri"`
	TargetRange          lspRange `json:"targetRange"`
	TargetSelectionRange lspRange `json:"targetSelectionRange"`
}

func parseResult(operation string, raw json.RawMessage, workspace string) (Result, error) {
	result := Result{Operation: operation}
	switch operation {
	case "definition", "references", "implementation", "typeDefinition":
		locations, err := parseLocations(raw, workspace)
		if err != nil {
			return Result{}, err
		}
		result.Locations = locations
	case "hover":
		hover, err := parseHover(raw)
		if err != nil {
			return Result{}, err
		}
		result.Hover = hover
	case "documentSymbol", "workspaceSymbol":
		symbols, err := parseSymbols(raw, workspace, operation == "workspaceSymbol")
		if err != nil {
			return Result{}, err
		}
		result.Symbols = symbols
	case "diagnostics":
		diagnostics, err := parseDiagnostics(raw)
		if err != nil {
			return Result{}, err
		}
		result.Diagnostics = diagnostics
	}
	return result, nil
}

func parseLocations(raw json.RawMessage, workspace string) ([]Location, error) {
	if string(raw) == "null" {
		return []Location{}, nil
	}
	var many []json.RawMessage
	if err := json.Unmarshal(raw, &many); err == nil {
		locations := make([]Location, 0, len(many))
		for _, item := range many {
			location, ok, err := parseLocation(item, workspace)
			if err != nil {
				return nil, err
			}
			if ok {
				locations = append(locations, location)
			}
		}
		return locations, nil
	}
	location, ok, err := parseLocation(raw, workspace)
	if err != nil {
		return nil, err
	}
	if !ok {
		return []Location{}, nil
	}
	return []Location{location}, nil
}

func parseLocation(raw json.RawMessage, workspace string) (Location, bool, error) {
	var direct lspLocation
	if err := json.Unmarshal(raw, &direct); err != nil {
		return Location{}, false, err
	}
	if direct.URI != "" {
		path, ok := confinedURI(workspace, direct.URI)
		if !ok {
			return Location{}, false, nil
		}
		return Location{FilePath: path, Range: stableRange(direct.Range)}, true, nil
	}
	var link lspLocationLink
	if err := json.Unmarshal(raw, &link); err != nil {
		return Location{}, false, err
	}
	path, ok := confinedURI(workspace, link.TargetURI)
	if !ok {
		return Location{}, false, nil
	}
	return Location{FilePath: path, Range: stableRange(link.TargetSelectionRange)}, true, nil
}

func confinedURI(workspace, rawURI string) (string, bool) {
	uri, err := url.Parse(rawURI)
	if err != nil || uri.Scheme != "file" || uri.Host != "" || uri.RawQuery != "" || uri.Fragment != "" {
		return "", false
	}
	path := filepath.FromSlash(uri.Path)
	if !filepath.IsAbs(path) {
		return "", false
	}
	resolved, err := pathutil.ResolveWorkspace(workspace, path)
	if err != nil {
		return "", false
	}
	return resolved, true
}

func stableRange(r lspRange) Range {
	return Range{
		Start: Position{Line: r.Start.Line + 1, Character: r.Start.Character + 1},
		End:   Position{Line: r.End.Line + 1, Character: r.End.Character + 1},
	}
}

func parseHover(raw json.RawMessage) (*Hover, error) {
	if string(raw) == "null" {
		return nil, nil
	}
	var value struct {
		Contents json.RawMessage `json:"contents"`
		Range    *lspRange       `json:"range"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	contents, err := hoverContents(value.Contents)
	if err != nil {
		return nil, err
	}
	hover := &Hover{Contents: contents}
	if value.Range != nil {
		resultRange := stableRange(*value.Range)
		hover.Range = &resultRange
	}
	return hover, nil
}

func hoverContents(raw json.RawMessage) (string, error) {
	return hoverContentsDepth(raw, 0)
}

func hoverContentsDepth(raw json.RawMessage, depth int) (string, error) {
	if depth > 32 {
		return "", fmt.Errorf("LSP hover content nesting exceeds 32 levels")
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var markup struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &markup); err == nil && markup.Value != "" {
		return markup.Value, nil
	}
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return "", err
	}
	parts := make([]string, 0, len(values))
	for _, value := range values {
		part, err := hoverContentsDepth(value, depth+1)
		if err != nil {
			return "", err
		}
		if part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

func parseSymbols(raw json.RawMessage, workspace string, workspaceSymbols bool) ([]Symbol, error) {
	nodes := 0
	return parseSymbolsDepth(raw, workspace, workspaceSymbols, 0, &nodes)
}

func parseSymbolsDepth(raw json.RawMessage, workspace string, workspaceSymbols bool, depth int, nodes *int) ([]Symbol, error) {
	if depth > 64 {
		return nil, fmt.Errorf("LSP symbol nesting exceeds 64 levels")
	}
	if string(raw) == "null" {
		return []Symbol{}, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	*nodes += len(items)
	if *nodes > 10000 {
		return nil, fmt.Errorf("LSP symbol result exceeds 10000 nodes")
	}
	symbols := make([]Symbol, 0, len(items))
	for _, item := range items {
		var information struct {
			Name     string          `json:"name"`
			Kind     int             `json:"kind"`
			Detail   string          `json:"containerName"`
			Location json.RawMessage `json:"location"`
		}
		if err := json.Unmarshal(item, &information); err != nil {
			return nil, err
		}
		if workspaceSymbols || len(information.Location) != 0 {
			if len(information.Location) == 0 {
				return nil, fmt.Errorf("symbol %q has no location", information.Name)
			}
			location, ok, err := parseLocation(information.Location, workspace)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			symbols = append(symbols, Symbol{Name: information.Name, Kind: information.Kind, Detail: information.Detail, Location: &location})
			continue
		}
		var document struct {
			Name           string            `json:"name"`
			Detail         string            `json:"detail"`
			Kind           int               `json:"kind"`
			Range          *lspRange         `json:"range"`
			SelectionRange *lspRange         `json:"selectionRange"`
			Children       []json.RawMessage `json:"children"`
		}
		if err := json.Unmarshal(item, &document); err != nil {
			return nil, err
		}
		if document.Range == nil || document.SelectionRange == nil {
			return nil, fmt.Errorf("document symbol %q has no range", document.Name)
		}
		symbolRange := stableRange(*document.Range)
		selectionRange := stableRange(*document.SelectionRange)
		symbol := Symbol{Name: document.Name, Kind: document.Kind, Detail: document.Detail, Range: &symbolRange, SelectionRange: &selectionRange}
		for _, child := range document.Children {
			children, err := parseSymbolsDepth(json.RawMessage("["+string(child)+"]"), workspace, false, depth+1, nodes)
			if err != nil {
				return nil, err
			}
			symbol.Children = append(symbol.Children, children...)
		}
		symbols = append(symbols, symbol)
	}
	return symbols, nil
}

func parseDiagnostics(raw json.RawMessage) ([]Diagnostic, error) {
	if string(raw) == "null" {
		return []Diagnostic{}, nil
	}
	items := raw
	if strings.HasPrefix(strings.TrimSpace(string(raw)), "{") {
		var report struct {
			Items json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(raw, &report); err != nil {
			return nil, err
		}
		if len(report.Items) != 0 {
			items = report.Items
		}
	}
	var values []struct {
		Range    lspRange        `json:"range"`
		Severity int             `json:"severity"`
		Code     json.RawMessage `json:"code"`
		Source   string          `json:"source"`
		Message  string          `json:"message"`
	}
	if err := json.Unmarshal(items, &values); err != nil {
		return nil, err
	}
	diagnostics := make([]Diagnostic, 0, len(values))
	for _, value := range values {
		diagnostic := Diagnostic{
			Range:    stableRange(value.Range),
			Severity: value.Severity,
			Source:   value.Source,
			Message:  value.Message,
		}
		if len(value.Code) != 0 && string(value.Code) != "null" {
			if err := json.Unmarshal(value.Code, &diagnostic.Code); err != nil {
				var number json.Number
				if err := json.Unmarshal(value.Code, &number); err == nil {
					diagnostic.Code = number.String()
				}
			}
		}
		diagnostics = append(diagnostics, diagnostic)
	}
	return diagnostics, nil
}
