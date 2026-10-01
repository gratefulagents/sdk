package projectstate

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// DuplicateThreshold is the Similarity at or above which a new memory is
// considered a likely duplicate of an existing one.
const DuplicateThreshold = 0.55

const (
	legacyTitleMaxRunes = 100
	defaultSearchLimit  = 8
	// briefingRecencyUnit converts absolute timestamps into usage-score
	// points: one recorded use is worth one unit of recency.
	briefingRecencyUnit = 7 * 24 * time.Hour
)

// NormalizeMemoryKind lowercases and trims kind and maps legacy v1 kinds onto
// v2 kinds. Empty or unknown kinds become MemoryKindFact.
func NormalizeMemoryKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case MemoryKindPreference:
		return MemoryKindPreference
	case MemoryKindDecision, "pinned":
		return MemoryKindDecision
	case MemoryKindProcedure, "procedural":
		return MemoryKindProcedure
	default:
		return MemoryKindFact
	}
}

// LegacyMemoryTitle derives a title from v1 free-form memory content: the
// first line, cut at the first sentence end, whitespace collapsed, and at most
// 100 runes (with "..." when truncated).
func LegacyMemoryTitle(content string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(content), "\n")
	if i := strings.Index(first, ". "); i >= 0 {
		first = first[:i+1]
	}
	return truncateRunes(collapseSpace(first), legacyTitleMaxRunes)
}

// ValidateMemoryInput trims and normalizes in place, then checks the title and
// body limits and the citations. Error messages are written for the model.
func ValidateMemoryInput(in *SaveMemoryInput) error {
	if in == nil {
		return errors.New("memory input is required")
	}
	in.ID = strings.TrimSpace(in.ID)
	in.Kind = NormalizeMemoryKind(in.Kind)
	in.Title = collapseSpace(in.Title)
	in.Body = strings.TrimSpace(in.Body)
	in.CommitSHA = strings.TrimSpace(in.CommitSHA)
	in.SourceRun = strings.TrimSpace(in.SourceRun)
	if in.Title == "" {
		return errors.New("memory title is required: give a one-line summary of the knowledge")
	}
	if n := utf8.RuneCountInString(in.Title); n > MaxMemoryTitleLen {
		return fmt.Errorf("memory title is %d characters; the limit is %d. Shorten it to a one-line summary and move details into the body", n, MaxMemoryTitleLen)
	}
	if in.Body == "" {
		return errors.New("memory body is required: state the reusable knowledge and, for decisions, the rationale")
	}
	if n := utf8.RuneCountInString(in.Body); n > MaxMemoryBodyLen {
		return fmt.Errorf("memory body is %d characters; the limit is %d. Shorten it to the durable lesson, or split it into several focused memories", n, MaxMemoryBodyLen)
	}
	citations := make([]Citation, 0, len(in.Citations))
	seen := map[Citation]bool{}
	for _, c := range in.Citations {
		c.Path = strings.TrimSpace(c.Path)
		c.URL = strings.TrimSpace(c.URL)
		if c.Path == "" && c.URL == "" {
			continue
		}
		if c.Path != "" {
			if filepath.IsAbs(c.Path) || strings.HasPrefix(c.Path, "/") || strings.HasPrefix(c.Path, `\`) {
				return fmt.Errorf("citation path %q must be workspace-relative, not absolute", c.Path)
			}
			if strings.Contains(c.Path, "..") {
				return fmt.Errorf("citation path %q must not contain \"..\"", c.Path)
			}
			c.Path = filepath.ToSlash(filepath.Clean(c.Path))
		}
		if seen[c] {
			continue
		}
		seen[c] = true
		citations = append(citations, c)
	}
	if len(citations) == 0 {
		citations = nil
	}
	in.Citations = citations
	return nil
}

var (
	progressPRRef   = regexp.MustCompile(`(?i)\bPR\s*#\s*\d+|pull/\d+|\bpull request\s*#?\s*\d+`)
	progressMerged  = regexp.MustCompile(`(?i)\bmerged\b`)
	progressCI      = regexp.MustCompile(`(?i)\bCI\b[^.\n]{0,24}\b(green|pass|passed|passes|passing)\b`)
	progressTests   = regexp.MustCompile(`(?i)\btests?\b[^.\n]{0,16}\b(passed|pass|passing|green)\b`)
	progressHexWord = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)
	hasDigit        = regexp.MustCompile(`[0-9]`)
	hasHexLetter    = regexp.MustCompile(`[a-f]`)
)

// ProgressLogWarning returns a non-empty warning when the text reads like a
// progress log or changelog (PR references, merge/CI status, commit hashes,
// test results) rather than reusable knowledge. It returns "" otherwise.
func ProgressLogWarning(title, body string) string {
	text := title + "\n" + body
	signals := 0
	for _, re := range []*regexp.Regexp{progressPRRef, progressMerged, progressCI, progressTests} {
		if re.MatchString(text) {
			signals++
		}
	}
	for _, word := range progressHexWord.FindAllString(text, -1) {
		if hasDigit.MatchString(word) && hasHexLetter.MatchString(word) {
			signals++
			break
		}
	}
	if signals < 2 {
		return ""
	}
	return "This reads like a progress log or changelog (PR/merge/CI status, commit hashes). " +
		"Memories should hold reusable knowledge for future runs — preferences, decisions with rationale, gotchas, procedures — " +
		"not a record of what was done. Rewrite it as the durable lesson or delete it with memory_delete."
}

var stopwords = map[string]bool{
	"an": true, "the": true, "and": true, "or": true, "of": true, "to": true, "in": true,
	"on": true, "for": true, "with": true, "is": true, "are": true, "was": true, "were": true,
	"be": true, "been": true, "it": true, "its": true, "this": true, "that": true, "these": true,
	"those": true, "as": true, "at": true, "by": true, "from": true, "not": true, "but": true,
	"if": true, "then": true, "so": true, "do": true, "does": true, "we": true, "you": true,
	"they": true, "our": true, "your": true, "my": true, "me": true, "us": true, "can": true,
	"will": true, "should": true, "would": true, "could": true, "has": true, "have": true,
	"had": true, "into": true, "than": true, "there": true, "what": true, "when": true,
	"where": true, "which": true, "who": true, "how": true, "why": true, "also": true,
}

// Tokenize lowercases text and splits it on non-alphanumerics. Tokens joined
// by '_' or '-' are kept whole and also split into their parts. Tokens shorter
// than two runes and common English stopwords are dropped; the result is
// deduplicated in first-seen order.
func Tokenize(text string) []string {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-'
	})
	var out []string
	seen := map[string]bool{}
	add := func(tok string) {
		tok = strings.Trim(tok, "_-")
		if utf8.RuneCountInString(tok) < 2 || stopwords[tok] || seen[tok] {
			return
		}
		seen[tok] = true
		out = append(out, tok)
	}
	for _, field := range fields {
		add(field)
		if strings.ContainsAny(field, "_-") {
			for _, part := range strings.FieldsFunc(field, func(r rune) bool { return r == '_' || r == '-' }) {
				add(part)
			}
		}
	}
	return out
}

// LexicalScore is the weighted fraction of distinct query tokens found in the
// memory, in [0,1]. A title match weighs 1.0; otherwise a body or citation
// path/URL match weighs 0.6.
func LexicalScore(query string, m Memory) float64 {
	terms := Tokenize(query)
	if len(terms) == 0 {
		return 0
	}
	title := tokenSet(m.Title)
	body := tokenSet(m.Body)
	var refs []string
	for _, c := range m.Citations {
		refs = append(refs, c.Path, c.URL)
	}
	cited := tokenSet(strings.Join(refs, " "))
	var total float64
	for _, term := range terms {
		switch {
		case title[term]:
			total += 1.0
		case body[term] || cited[term]:
			total += 0.6
		}
	}
	return total / float64(len(terms))
}

// Similarity is the Jaccard similarity of the token sets of two title+body
// pairs, in [0,1].
func Similarity(aTitle, aBody, bTitle, bBody string) float64 {
	a := tokenSet(aTitle + " " + aBody)
	b := tokenSet(bTitle + " " + bBody)
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for tok := range a {
		if b[tok] {
			inter++
		}
	}
	return float64(inter) / float64(len(a)+len(b)-inter)
}

func tokenSet(text string) map[string]bool {
	toks := Tokenize(text)
	set := make(map[string]bool, len(toks))
	for _, tok := range toks {
		set[tok] = true
	}
	return set
}

// SelectBriefingMemories picks the memories listed in the briefing index:
// every preference and decision (newest UpdatedAt first), then facts and
// procedures by usage score (UseCount plus a recency bonus from the later of
// LastUsedAt and VerifiedAt), until the rendered index lines would exceed
// budget bytes. budget <= 0 means BriefingMemoryBudget. The result depends
// only on the input, never on the current time.
func SelectBriefingMemories(all []Memory, budget int) []Memory {
	if budget <= 0 {
		budget = BriefingMemoryBudget
	}
	var core, rest []Memory
	for _, m := range all {
		switch NormalizeMemoryKind(m.Kind) {
		case MemoryKindPreference, MemoryKindDecision:
			core = append(core, m)
		default:
			rest = append(rest, m)
		}
	}
	sort.SliceStable(core, func(i, j int) bool {
		if !core[i].UpdatedAt.Equal(core[j].UpdatedAt) {
			return core[i].UpdatedAt.After(core[j].UpdatedAt)
		}
		return core[i].ID < core[j].ID
	})
	sort.SliceStable(rest, func(i, j int) bool {
		si, sj := briefingUsageScore(rest[i]), briefingUsageScore(rest[j])
		if si != sj {
			return si > sj
		}
		return rest[i].ID < rest[j].ID
	})
	var out []Memory
	used := 0
	for _, m := range append(core, rest...) {
		n := len(memoryIndexLine(m))
		if used+n > budget {
			break
		}
		used += n
		out = append(out, m)
	}
	return out
}

func briefingUsageScore(m Memory) float64 {
	ts := m.VerifiedAt
	if m.LastUsedAt != nil && m.LastUsedAt.After(ts) {
		ts = *m.LastUsedAt
	}
	score := float64(m.UseCount)
	if !ts.IsZero() {
		score += float64(ts.Unix()) / briefingRecencyUnit.Seconds()
	}
	return score
}

func memoryIndexLine(m Memory) string {
	return "- " + m.ID + " [" + NormalizeMemoryKind(m.Kind) + "] " + collapseSpace(m.Title) + "\n"
}

// BriefingInput is everything RenderBriefing needs.
type BriefingInput struct {
	ProjectID string
	Active    *Task
	Ready     []Task
	Blocked   []Task
	// Memories holds all project memories; RenderBriefing selects the index.
	Memories []Memory
}

// RenderBriefing renders the durable-state briefing as deterministic markdown
// without timestamps, so identical state yields identical, cache-friendly text.
func RenderBriefing(in BriefingInput) string {
	var b strings.Builder
	b.WriteString("## Durable Project State\n")
	if in.ProjectID != "" {
		b.WriteString("Project: " + in.ProjectID + "\n")
	}
	empty := true
	if in.Active != nil {
		empty = false
		b.WriteString("\n### Active Task\n")
		writeTaskLine(&b, *in.Active)
		if in.Active.Description != "" {
			b.WriteString("  " + oneLine(in.Active.Description, 220) + "\n")
		}
		if len(in.Active.DependsOn) > 0 {
			b.WriteString("  Depends on: " + strings.Join(in.Active.DependsOn, ", ") + "\n")
		}
	}
	if len(in.Ready) > 0 {
		empty = false
		b.WriteString("\n### Ready Work\n")
		for _, task := range in.Ready {
			writeTaskLine(&b, task)
		}
	}
	if len(in.Blocked) > 0 {
		empty = false
		b.WriteString("\n### Blocked Work\n")
		for _, task := range in.Blocked {
			writeTaskLine(&b, task)
		}
	}
	if len(in.Memories) > 0 {
		empty = false
		shown := SelectBriefingMemories(in.Memories, BriefingMemoryBudget)
		b.WriteString("\n### Memory Index (" + strconv.Itoa(len(shown)) + " of " + strconv.Itoa(len(in.Memories)) +
			") — memory_get <id> for full text; memory_search for anything not listed\n")
		for _, m := range shown {
			b.WriteString(memoryIndexLine(m))
		}
	}
	out := strings.TrimSpace(b.String())
	if empty {
		out += "\nNo durable tasks or memories yet."
	}
	return out
}

// VectorLiteral renders v as a pgvector literal such as "[0.1,0.2]".
func VectorLiteral(v []float32) string {
	if len(v) == 0 {
		return "[]"
	}
	parts := make([]string, len(v))
	for i, f := range v {
		parts[i] = fmt.Sprintf("%g", f)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:max-3])) + "..."
}
