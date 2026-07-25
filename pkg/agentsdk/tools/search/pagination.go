package search

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/gratefulagents/sdk/pkg/agentsdk/tools/internal/pathutil"
)

const (
	cursorVersion         = 1
	maxSearchPageSize     = 1000
	maxSearchContext      = 10
	maxSearchFileBytes    = 10 << 20
	maxSearchPatternBytes = 4096
	maxOmittedFiles       = 100
	maxGitignoreBytes     = 1 << 20
	maxGitignoreRules     = 10000
)

type searchPage struct {
	Matches               any      `json:"matches"`
	Truncated             bool     `json:"truncated"`
	NextCursor            string   `json:"next_cursor,omitempty"`
	Incomplete            bool     `json:"incomplete"`
	OmittedFiles          []string `json:"omitted_files,omitempty"`
	OmittedFilesTruncated bool     `json:"omitted_files_truncated,omitempty"`
}

type searchCursor struct {
	Version int    `json:"v"`
	Query   string `json:"q"`
	Offset  int    `json:"o"`
}

type pathFilters struct {
	Includes          []string
	Excludes          []string
	RespectGitignore  bool
	SkipDefaultDirs   bool
	workspaceRoot     string
	gitignoreMatchers []ignoreMatcher
}

type ignoreMatcher struct {
	base     string
	pattern  string
	negated  bool
	dirOnly  bool
	anchored bool
}

type grepMatch struct {
	Path   string   `json:"path"`
	Line   int      `json:"line"`
	Text   string   `json:"text"`
	Before []string `json:"before,omitempty"`
	After  []string `json:"after,omitempty"`
}

type grepCount struct {
	Path  string `json:"path"`
	Count int    `json:"count"`
}

func queryFingerprint(kind string, value any) string {
	data, _ := json.Marshal(struct {
		Kind  string `json:"kind"`
		Value any    `json:"value"`
	}{kind, value})
	sum := sha256.Sum256(data)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func decodeCursor(raw, query string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return 0, errors.New("invalid cursor")
	}
	var cursor searchCursor
	if json.Unmarshal(data, &cursor) != nil || cursor.Version != cursorVersion || cursor.Offset < 0 {
		return 0, errors.New("invalid cursor")
	}
	if cursor.Query != query {
		return 0, errors.New("cursor does not match this search")
	}
	return cursor.Offset, nil
}

func encodeCursor(query string, offset int) string {
	data, _ := json.Marshal(searchCursor{Version: cursorVersion, Query: query, Offset: offset})
	return base64.RawURLEncoding.EncodeToString(data)
}

func structuredPage(matches any, truncated bool, next string, omitted []string, omittedTruncated bool) (string, error) {
	data, err := json.Marshal(searchPage{
		Matches: matches, Truncated: truncated, NextCursor: next,
		Incomplete: len(omitted) > 0 || omittedTruncated, OmittedFiles: omitted,
		OmittedFilesTruncated: omittedTruncated,
	})
	return string(data), err
}

func appendTextMetadata(content string, matchCount int, truncated bool, next string) string {
	if !truncated {
		return content
	}
	metadata, _ := json.Marshal(struct {
		Matches    int    `json:"matches"`
		Truncated  bool   `json:"truncated"`
		NextCursor string `json:"next_cursor"`
	}{matchCount, true, next})
	if content != "" {
		content += "\n"
	}
	return content + "[search_metadata " + string(metadata) + "]"
}

func validatePatterns(patterns []string, label string) error {
	for _, pattern := range patterns {
		if len(pattern) > maxSearchPatternBytes {
			return fmt.Errorf("%s pattern exceeds %d bytes", label, maxSearchPatternBytes)
		}
		if _, err := filepath.Match(pattern, ""); err != nil {
			return fmt.Errorf("invalid %s pattern %q: %w", label, pattern, err)
		}
	}
	return nil
}

func newPathFilters(root string, includes, excludes []string, respectGitignore, skipDefaultDirs bool) (pathFilters, error) {
	if err := validatePatterns(includes, "include"); err != nil {
		return pathFilters{}, err
	}
	if err := validatePatterns(excludes, "exclude"); err != nil {
		return pathFilters{}, err
	}
	filters := pathFilters{
		Includes: includes, Excludes: excludes, RespectGitignore: respectGitignore,
		SkipDefaultDirs: skipDefaultDirs, workspaceRoot: root,
	}
	if respectGitignore {
		filters.gitignoreMatchers = loadGitignoreMatchers(root, skipDefaultDirs)
	}
	return filters, nil
}

func (f pathFilters) skipDir(path string, d os.DirEntry) bool {
	if path == f.workspaceRoot {
		return false
	}
	if f.SkipDefaultDirs && shouldSkipDir(d.Name()) {
		return true
	}
	// Do not prune gitignored directories: a later negated rule may re-include
	// descendants. Individual files are filtered by include.
	return false
}

func (f pathFilters) include(path string) bool {
	rel, err := filepath.Rel(f.workspaceRoot, path)
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	if f.ignored(rel, false) {
		return false
	}
	if len(f.Includes) > 0 && !matchesAny(f.Includes, rel) {
		return false
	}
	return !matchesAny(f.Excludes, rel)
}

func matchesAny(patterns []string, rel string) bool {
	for _, pattern := range patterns {
		pattern = filepath.ToSlash(pattern)
		if doubleStarMatch(pattern, rel) {
			return true
		}
		if ok, _ := filepath.Match(pattern, filepath.Base(rel)); ok {
			return true
		}
	}
	return false
}

func (f pathFilters) ignored(rel string, isDir bool) bool {
	if !f.RespectGitignore {
		return false
	}
	ignored := false
	for _, matcher := range f.gitignoreMatchers {
		candidate := rel
		if matcher.base != "." {
			prefix := strings.TrimSuffix(matcher.base, "/") + "/"
			if !strings.HasPrefix(candidate, prefix) {
				continue
			}
			candidate = strings.TrimPrefix(candidate, prefix)
		}
		components := strings.Split(candidate, "/")
		directoryComponents := len(components)
		if !isDir {
			directoryComponents--
		}
		candidates := make([]string, 0, len(components))
		for i := 0; i < directoryComponents; i++ {
			candidates = append(candidates, strings.Join(components[:i+1], "/"))
		}
		if !matcher.dirOnly && !isDir {
			candidates = append(candidates, candidate)
		}
		matched := false
		for _, value := range candidates {
			if matcher.anchored || strings.Contains(matcher.pattern, "/") {
				matched = doubleStarMatch(matcher.pattern, value)
			} else {
				for _, component := range strings.Split(value, "/") {
					if ok, _ := filepath.Match(matcher.pattern, component); ok {
						matched = true
						break
					}
				}
			}
			if matched {
				break
			}
		}
		if matched {
			ignored = !matcher.negated
		}
	}
	return ignored
}

func loadGitignoreMatchers(root string, skipDefaultDirs bool) []ignoreMatcher {
	var matchers []ignoreMatcher
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != root && skipDefaultDirs && shouldSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != ".gitignore" {
			return nil
		}
		f, err := pathutil.OpenInWorkspace(root, path, os.O_RDONLY, 0)
		if err != nil {
			return nil
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxGitignoreBytes || pathutil.RequireSingleLink(info) != nil {
			_ = f.Close()
			return nil
		}
		data, err := io.ReadAll(io.LimitReader(f, maxGitignoreBytes+1))
		_ = f.Close()
		if err != nil || len(data) > maxGitignoreBytes {
			return nil
		}
		base, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return nil
		}
		base = filepath.ToSlash(base)
		for _, line := range strings.Split(string(data), "\n") {
			if len(matchers) >= maxGitignoreRules {
				return filepath.SkipAll
			}
			line = strings.TrimSpace(line)
			if line == "" || len(line) > maxSearchPatternBytes || strings.HasPrefix(line, "#") {
				continue
			}
			matcher := ignoreMatcher{base: base}
			if strings.HasPrefix(line, "!") {
				matcher.negated = true
				line = strings.TrimPrefix(line, "!")
			}
			matcher.anchored = strings.HasPrefix(line, "/")
			line = strings.TrimPrefix(line, "/")
			matcher.dirOnly = strings.HasSuffix(line, "/")
			matcher.pattern = strings.TrimSuffix(line, "/")
			if matcher.pattern != "" {
				matchers = append(matchers, matcher)
			}
		}
		return nil
	})
	return matchers
}

func collectGlob(root, pattern string, filters pathFilters, offset, limit int) ([]string, bool, error) {
	matches := make([]string, 0, limit+1)
	seen := 0
	stop := errors.New("page complete")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if filters.skipDir(path, d) {
				return filepath.SkipDir
			}
			return nil
		}
		if !filters.include(path) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		matched := doubleStarMatch(filepath.ToSlash(pattern), rel)
		if !strings.Contains(pattern, "**") {
			matched, err = filepath.Match(pattern, filepath.Base(rel))
			if err == nil && !matched {
				matched, err = filepath.Match(pattern, rel)
			}
		}
		if err != nil {
			return err
		}
		if matched {
			if seen < offset {
				seen++
				return nil
			}
			workspaceRel, relErr := filepath.Rel(filters.workspaceRoot, path)
			if relErr != nil {
				return relErr
			}
			matches = append(matches, filepath.ToSlash(workspaceRel))
			if len(matches) > limit {
				return stop
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, stop) {
		return nil, false, err
	}
	truncated := len(matches) > limit
	if truncated {
		matches = matches[:limit]
	}
	return matches, truncated, nil
}

type grepCollection struct {
	Matches          []grepMatch
	Files            []string
	Counts           []grepCount
	Omitted          []string
	OmittedTruncated bool
	Truncated        bool
}

func collectGrep(workDir, root, base string, re *regexp.Regexp, filters pathFilters, before, after int, mode string, offset, limit int) (grepCollection, error) {
	result := grepCollection{
		Matches: make([]grepMatch, 0), Files: make([]string, 0),
		Counts: make([]grepCount, 0), Omitted: make([]string, 0),
	}
	addOmitted := func(message string) {
		if len(result.Omitted) < maxOmittedFiles {
			result.Omitted = append(result.Omitted, message)
		} else {
			result.OmittedTruncated = true
		}
	}
	seen := 0
	stop := errors.New("page complete")
	addUnit := func(match grepMatch, path string, count int) error {
		if seen < offset {
			seen++
			return nil
		}
		switch mode {
		case "files":
			result.Files = append(result.Files, path)
		case "count":
			result.Counts = append(result.Counts, grepCount{Path: path, Count: count})
		default:
			result.Matches = append(result.Matches, match)
		}
		seen++
		pageLen := len(result.Matches) + len(result.Files) + len(result.Counts)
		if pageLen > limit {
			result.Truncated = true
			return stop
		}
		return nil
	}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if filters.skipDir(path, d) {
				return filepath.SkipDir
			}
			return nil
		}
		if !filters.include(path) {
			return nil
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		f, err := pathutil.OpenInWorkspace(workDir, path, os.O_RDONLY, 0)
		if err != nil {
			addOmitted(rel + ": cannot open")
			return nil
		}
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			addOmitted(rel + ": cannot stat")
			return nil
		}
		if !info.Mode().IsRegular() {
			_ = f.Close()
			return nil
		}
		if err := pathutil.RequireSingleLink(info); err != nil {
			_ = f.Close()
			addOmitted(rel + ": hard-linked file refused")
			return nil
		}
		if info.Size() > maxSearchFileBytes {
			_ = f.Close()
			addOmitted(rel + ": file exceeds 10 MiB search bound")
			return nil
		}
		limited := &io.LimitedReader{R: f, N: maxSearchFileBytes + 1}
		scanner := bufio.NewScanner(limited)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		lines := make([]string, 0)
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
		}
		scanErr := scanner.Err()
		grewPastLimit := limited.N == 0
		if closeErr := f.Close(); closeErr != nil {
			return fmt.Errorf("closing %s: %w", path, closeErr)
		}
		if scanErr != nil {
			if errors.Is(scanErr, bufio.ErrTooLong) {
				addOmitted(rel + ": line too long")
			} else {
				return fmt.Errorf("scanning %s: %w", path, scanErr)
			}
		}
		if grewPastLimit {
			addOmitted(rel + ": file grew past 10 MiB search bound")
			if len(lines) > 0 {
				lines = lines[:len(lines)-1]
			}
		}
		fileMatchCount := 0
		for i, line := range lines {
			if !re.MatchString(line) {
				continue
			}
			start := i - before
			if start < 0 {
				start = 0
			}
			end := i + after + 1
			if end < i || end > len(lines) {
				end = len(lines)
			}
			match := grepMatch{Path: rel, Line: i + 1, Text: boundLine(line)}
			for _, contextLine := range lines[start:i] {
				match.Before = append(match.Before, boundLine(contextLine))
			}
			for _, contextLine := range lines[i+1 : end] {
				match.After = append(match.After, boundLine(contextLine))
			}
			fileMatchCount++
			if mode == "matches" {
				if err := addUnit(match, "", 0); err != nil {
					return err
				}
			}
		}
		if fileMatchCount > 0 && mode == "files" {
			return addUnit(grepMatch{}, rel, 0)
		}
		if fileMatchCount > 0 && mode == "count" {
			return addUnit(grepMatch{}, rel, fileMatchCount)
		}
		return nil
	})
	if err != nil && !errors.Is(err, stop) {
		return grepCollection{}, err
	}
	if result.Truncated {
		switch mode {
		case "files":
			result.Files = result.Files[:limit]
		case "count":
			result.Counts = result.Counts[:limit]
		default:
			result.Matches = result.Matches[:limit]
		}
	}
	sort.Strings(result.Omitted)
	return result, nil
}

func boundLine(line string) string {
	if len(line) > maxLineBytes {
		return line[:maxLineBytes] + "..."
	}
	return line
}

func formatGrepText(matches []grepMatch, before, after int) string {
	var b strings.Builder
	for index, match := range matches {
		if index > 0 && (before > 0 || after > 0) {
			b.WriteString("--\n")
		}
		firstLine := match.Line - len(match.Before)
		for i, line := range match.Before {
			fmt.Fprintf(&b, "%s-%d- %s\n", match.Path, firstLine+i, line)
		}
		fmt.Fprintf(&b, "%s:%d: %s\n", match.Path, match.Line, match.Text)
		for i, line := range match.After {
			fmt.Fprintf(&b, "%s-%d- %s\n", match.Path, match.Line+i+1, line)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatCountsText(counts []grepCount) string {
	var b strings.Builder
	for _, count := range counts {
		fmt.Fprintf(&b, "%s: %d\n", count.Path, count.Count)
	}
	return strings.TrimRight(b.String(), "\n")
}
