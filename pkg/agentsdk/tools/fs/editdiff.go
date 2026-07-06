package fs

import (
	"fmt"
	"sort"
	"strings"
)

// The Edit tool appends a unified diff of what changed to its success
// message. The diff serves two consumers: the model gets positive
// confirmation (with line numbers and context) that the edit landed as
// intended, and host UIs receive it through the tool_end event stream so
// they can visualize the change (e.g. the dashboard activity log).
const (
	// editDiffContextLines is the number of unchanged lines shown around
	// each change, matching standard diff -U3 output.
	editDiffContextLines = 3
	// maxEditDiffBytes caps the rendered diff so huge edits don't bloat
	// model context or the event stream.
	maxEditDiffBytes = 8 * 1024
	// maxEditDiffMatches caps how many replace_all occurrences are rendered.
	// Anything beyond this exceeds maxEditDiffBytes anyway.
	maxEditDiffMatches = 256
	// editDiffTruncationNote marks a diff cut off by the caps above.
	editDiffTruncationNote = "... [diff truncated]"
)

// buildEditDiff renders a unified diff (@@ hunk headers, -/+/space line
// prefixes, editDiffContextLines of context) of replacing oldStr with newStr
// in content. When replaceAll is false only the first occurrence is
// rendered, mirroring strings.Replace(content, oldStr, newStr, 1). Nearby
// changes merge into a single hunk. Returns "" when oldStr does not occur.
func buildEditDiff(content, oldStr, newStr string, replaceAll bool) string {
	offsets, offsetsTruncated := editMatchOffsets(content, oldStr, replaceAll)
	if len(offsets) == 0 {
		return ""
	}
	li := newLineIndex(content)
	runs := buildEditRuns(li, offsets, len(oldStr))

	// Group runs into hunks: runs separated by more than twice the context
	// width get their own hunk, mirroring standard diff behavior.
	var hunks [][]editRun
	for _, run := range runs {
		if n := len(hunks); n > 0 {
			prev := hunks[n-1][len(hunks[n-1])-1]
			if run.firstLine-prev.lastLine-1 <= 2*editDiffContextLines {
				hunks[n-1] = append(hunks[n-1], run)
				continue
			}
		}
		hunks = append(hunks, []editRun{run})
	}

	var b strings.Builder
	lineDelta := 0 // cumulative added-minus-removed lines from prior hunks
	truncated := offsetsTruncated
	for _, hunkRuns := range hunks {
		if b.Len() > maxEditDiffBytes {
			truncated = true
			break
		}
		lineDelta = writeEditHunk(&b, li, hunkRuns, oldStr, newStr, lineDelta)
	}
	diff := b.String()
	if len(diff) > maxEditDiffBytes {
		diff = truncateAtLineBoundary(diff, maxEditDiffBytes)
		truncated = true
	}
	diff = strings.TrimSuffix(diff, "\n")
	if truncated {
		diff += "\n" + editDiffTruncationNote
	}
	return diff
}

// editMatchOffsets returns the byte offsets of the non-overlapping,
// left-to-right occurrences of oldStr in content — the occurrences
// strings.Replace/ReplaceAll substitutes. When replaceAll is false only the
// first match is returned. The second result reports whether scanning
// stopped early at maxEditDiffMatches.
func editMatchOffsets(content, oldStr string, replaceAll bool) ([]int, bool) {
	if oldStr == "" {
		return nil, false
	}
	var offsets []int
	idx := 0
	for {
		j := strings.Index(content[idx:], oldStr)
		if j < 0 {
			return offsets, false
		}
		if len(offsets) == maxEditDiffMatches {
			return offsets, true
		}
		offsets = append(offsets, idx+j)
		idx += j + len(oldStr)
		if !replaceAll {
			return offsets, false
		}
	}
}

// lineIndex maps byte offsets in a file to 0-based line numbers.
type lineIndex struct {
	content string
	starts  []int // byte offset of the first byte of each line
}

func newLineIndex(content string) *lineIndex {
	starts := []int{0}
	for i := 0; i < len(content); i++ {
		// A trailing newline terminates the last line rather than starting
		// an empty phantom line.
		if content[i] == '\n' && i+1 < len(content) {
			starts = append(starts, i+1)
		}
	}
	return &lineIndex{content: content, starts: starts}
}

func (li *lineIndex) lineCount() int { return len(li.starts) }

// lineOf returns the 0-based line containing byte offset off.
func (li *lineIndex) lineOf(off int) int {
	return sort.Search(len(li.starts), func(i int) bool { return li.starts[i] > off }) - 1
}

// lineStart and lineEnd bound line i as the byte range [start, end),
// including its trailing newline when present.
func (li *lineIndex) lineStart(i int) int { return li.starts[i] }

func (li *lineIndex) lineEnd(i int) int {
	if i+1 < len(li.starts) {
		return li.starts[i+1]
	}
	return len(li.content)
}

// lineText returns line i without its trailing newline.
func (li *lineIndex) lineText(i int) string {
	return strings.TrimSuffix(li.content[li.lineStart(i):li.lineEnd(i)], "\n")
}

// editRun is a maximal group of replacements whose line-expanded ranges
// share lines and must therefore render as a single -/+ block.
type editRun struct {
	firstLine, lastLine int   // 0-based inclusive line range in the old content
	offsets             []int // match offsets applied within the range
}

func buildEditRuns(li *lineIndex, offsets []int, oldLen int) []editRun {
	var runs []editRun
	for _, off := range offsets {
		first := li.lineOf(off)
		last := li.lineOf(off + oldLen - 1)
		if n := len(runs); n > 0 && first <= runs[n-1].lastLine {
			if last > runs[n-1].lastLine {
				runs[n-1].lastLine = last
			}
			runs[n-1].offsets = append(runs[n-1].offsets, off)
			continue
		}
		runs = append(runs, editRun{firstLine: first, lastLine: last, offsets: []int{off}})
	}
	return runs
}

// runNewText applies the run's replacements to its line-expanded old range
// and returns the resulting replacement text.
func runNewText(li *lineIndex, run editRun, oldStr, newStr string) string {
	var b strings.Builder
	pos := li.lineStart(run.firstLine)
	for _, off := range run.offsets {
		b.WriteString(li.content[pos:off])
		b.WriteString(newStr)
		pos = off + len(oldStr)
	}
	b.WriteString(li.content[pos:li.lineEnd(run.lastLine)])
	return b.String()
}

// splitDiffLines splits region text into display lines, dropping the empty
// remainder after a trailing newline so it doesn't render as a phantom line.
func splitDiffLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// writeEditHunk renders one hunk (header plus body) covering runs, and
// returns the updated cumulative line delta for subsequent hunk headers.
func writeEditHunk(b *strings.Builder, li *lineIndex, runs []editRun, oldStr, newStr string, lineDelta int) int {
	hunkStart := runs[0].firstLine - editDiffContextLines
	if hunkStart < 0 {
		hunkStart = 0
	}
	hunkEnd := runs[len(runs)-1].lastLine + editDiffContextLines // inclusive
	if maxLine := li.lineCount() - 1; hunkEnd > maxLine {
		hunkEnd = maxLine
	}

	var body strings.Builder
	oldCount, newCount := 0, 0
	writeContext := func(from, to int) { // lines [from, to)
		for i := from; i < to; i++ {
			body.WriteString(" ")
			body.WriteString(li.lineText(i))
			body.WriteString("\n")
		}
		oldCount += to - from
		newCount += to - from
	}

	cursor := hunkStart
	for _, run := range runs {
		writeContext(cursor, run.firstLine)
		for i := run.firstLine; i <= run.lastLine; i++ {
			body.WriteString("-")
			body.WriteString(li.lineText(i))
			body.WriteString("\n")
			oldCount++
		}
		for _, line := range splitDiffLines(runNewText(li, run, oldStr, newStr)) {
			body.WriteString("+")
			body.WriteString(line)
			body.WriteString("\n")
			newCount++
		}
		cursor = run.lastLine + 1
	}
	writeContext(cursor, hunkEnd+1)

	oldStart := hunkStart + 1
	newStart := hunkStart + 1 + lineDelta
	if newCount == 0 {
		// Unified diff convention: an empty range points at the line before it.
		newStart--
	}
	fmt.Fprintf(b, "@@ -%d,%d +%d,%d @@\n", oldStart, oldCount, newStart, newCount)
	b.WriteString(body.String())
	return lineDelta + (newCount - oldCount)
}

// truncateAtLineBoundary cuts s to at most max bytes, preferring the last
// complete line so a partial diff line is never emitted.
func truncateAtLineBoundary(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := strings.LastIndexByte(s[:max], '\n')
	if cut <= 0 {
		return s[:max]
	}
	return s[:cut]
}
