package shell

import (
	"strings"
)

// Token is one lexical unit produced by tokenize. Operator tokens (";", "|",
// "||", "&", "&&", ">", ">>", "<", "<<", "<<<") have IsOperator set; word
// tokens carry their decoded Value (quotes/escapes resolved, $IFS expanded to
// a space, unknown $VAR expanded to empty string).
//
// HasCmdSub is true when the original word contained a $(...) or backtick
// command substitution. CmdSubBodies holds the inner text of each substitution
// for recursive scanning.
type token struct {
	Value        string
	IsOperator   bool
	HasCmdSub    bool
	CmdSubBodies []string
}

// tokenize lexes a POSIX-ish shell command into tokens. It is intentionally
// scoped to what the destructive-command classifier needs: quoting/escaping,
// $IFS expansion, ANSI-C $'...' decoding, and recognition of command
// substitution and the common control operators. It does not implement full
// shell semantics (no aliases, no parameter expansion edge cases, etc.).
func tokenize(input string) []token {
	var out []token
	var cur strings.Builder
	curHasCmdSub := false
	var curBodies []string
	inWord := false

	flush := func() {
		if !inWord {
			return
		}
		out = append(out, token{Value: cur.String(), HasCmdSub: curHasCmdSub, CmdSubBodies: curBodies})
		cur.Reset()
		curHasCmdSub = false
		curBodies = nil
		inWord = false
	}
	startWord := func() {
		inWord = true
	}
	pushOp := func(op string) {
		flush()
		out = append(out, token{Value: op, IsOperator: true})
	}

	r := []rune(input)
	i := 0
	for i < len(r) {
		c := r[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			flush()
			i++
		case c == '\\' && i+1 < len(r):
			startWord()
			cur.WriteRune(r[i+1])
			i += 2
		case c == '\'':
			// Single quotes: literal until next '.
			startWord()
			j := i + 1
			for j < len(r) && r[j] != '\'' {
				cur.WriteRune(r[j])
				j++
			}
			if j < len(r) {
				j++
			}
			i = j
		case c == '"':
			// Double quotes: backslash escapes only \ " $ ` and newline; $VAR
			// and $() are still expanded.
			startWord()
			j := i + 1
			for j < len(r) {
				if r[j] == '"' {
					j++
					break
				}
				if r[j] == '\\' && j+1 < len(r) {
					next := r[j+1]
					if next == '\\' || next == '"' || next == '$' || next == '`' || next == '\n' {
						cur.WriteRune(next)
						j += 2
						continue
					}
				}
				if r[j] == '$' && j+1 < len(r) {
					consumed, body, isSub := decodeDollar(r, j)
					if consumed > 0 {
						if isSub {
							curHasCmdSub = true
							curBodies = append(curBodies, body)
						} else {
							cur.WriteString(body)
						}
						j += consumed
						continue
					}
				}
				if r[j] == '`' {
					j++
					var sub strings.Builder
					for j < len(r) && r[j] != '`' {
						if r[j] == '\\' && j+1 < len(r) {
							sub.WriteRune(r[j+1])
							j += 2
							continue
						}
						sub.WriteRune(r[j])
						j++
					}
					if j < len(r) {
						j++
					}
					curHasCmdSub = true
					curBodies = append(curBodies, sub.String())
					continue
				}
				cur.WriteRune(r[j])
				j++
			}
			i = j
		case c == '$':
			// $'...' ANSI-C, $(...) sub, ${VAR}, $VAR.
			consumed, body, isSub := decodeDollar(r, i)
			if consumed == 0 {
				startWord()
				cur.WriteRune(c)
				i++
				continue
			}
			if isSub {
				startWord()
				curHasCmdSub = true
				curBodies = append(curBodies, body)
			} else if body == " " && !inWord {
				// A bare $IFS expansion at a token boundary is whitespace.
				// inWord starts false here; just skip.
			} else if body == " " {
				// $IFS inside a word splits the word.
				flush()
			} else {
				startWord()
				cur.WriteString(body)
			}
			i += consumed
		case c == '`':
			startWord()
			j := i + 1
			var sub strings.Builder
			for j < len(r) && r[j] != '`' {
				if r[j] == '\\' && j+1 < len(r) {
					sub.WriteRune(r[j+1])
					j += 2
					continue
				}
				sub.WriteRune(r[j])
				j++
			}
			if j < len(r) {
				j++
			}
			curHasCmdSub = true
			curBodies = append(curBodies, sub.String())
			i = j
		case c == ';':
			pushOp(";")
			i++
		case c == '|':
			if i+1 < len(r) && r[i+1] == '|' {
				pushOp("||")
				i += 2
			} else {
				pushOp("|")
				i++
			}
		case c == '&':
			if i+1 < len(r) && r[i+1] == '&' {
				pushOp("&&")
				i += 2
			} else {
				pushOp("&")
				i++
			}
		case c == '>':
			if i+1 < len(r) && r[i+1] == '>' {
				pushOp(">>")
				i += 2
			} else {
				pushOp(">")
				i++
			}
		case c == '<':
			if i+2 < len(r) && r[i+1] == '<' && r[i+2] == '<' {
				pushOp("<<<")
				i += 3
			} else if i+1 < len(r) && r[i+1] == '<' {
				pushOp("<<")
				i += 2
			} else {
				pushOp("<")
				i++
			}
		default:
			startWord()
			cur.WriteRune(c)
			i++
		}
	}
	flush()
	return out
}

// decodeDollar handles a `$`-introduced sequence starting at r[i]. It returns
// (consumed runes, decoded body, isCommandSubstitution). consumed==0 means no
// match; caller should treat the `$` as a literal.
func decodeDollar(r []rune, i int) (int, string, bool) {
	if i >= len(r) || r[i] != '$' {
		return 0, "", false
	}
	if i+1 >= len(r) {
		return 0, "", false
	}
	next := r[i+1]
	switch {
	case next == '\'':
		// $'...' ANSI-C quoted string.
		j := i + 2
		var b strings.Builder
		for j < len(r) && r[j] != '\'' {
			if r[j] == '\\' && j+1 < len(r) {
				ch, n := decodeAnsiCEscape(r, j+1)
				b.WriteString(ch)
				j += 1 + n
				continue
			}
			b.WriteRune(r[j])
			j++
		}
		if j < len(r) {
			j++
		}
		return j - i, b.String(), false
	case next == '(':
		// $(...) command substitution; track nested parens.
		depth := 1
		j := i + 2
		var b strings.Builder
		for j < len(r) && depth > 0 {
			switch r[j] {
			case '(':
				depth++
				b.WriteRune(r[j])
			case ')':
				depth--
				if depth == 0 {
					j++
					return j - i, b.String(), true
				}
				b.WriteRune(r[j])
			case '\\':
				if j+1 < len(r) {
					b.WriteRune(r[j+1])
					j += 2
					continue
				}
				b.WriteRune(r[j])
			default:
				b.WriteRune(r[j])
			}
			j++
		}
		return j - i, b.String(), true
	case next == '{':
		// ${VAR}.
		j := i + 2
		var name strings.Builder
		for j < len(r) && r[j] != '}' {
			name.WriteRune(r[j])
			j++
		}
		if j < len(r) {
			j++
		}
		return j - i, expandVar(name.String()), false
	case isVarStart(next):
		j := i + 2
		for j < len(r) && isVarCont(r[j]) {
			j++
		}
		name := string(r[i+1 : j])
		return j - i, expandVar(name), false
	}
	return 0, "", false
}

func decodeAnsiCEscape(r []rune, i int) (string, int) {
	if i >= len(r) {
		return "\\", 0
	}
	c := r[i]
	switch c {
	case 'a':
		return "\a", 1
	case 'b':
		return "\b", 1
	case 'e', 'E':
		return "\x1b", 1
	case 'f':
		return "\f", 1
	case 'n':
		return "\n", 1
	case 'r':
		return "\r", 1
	case 't':
		return "\t", 1
	case 'v':
		return "\v", 1
	case '\\':
		return "\\", 1
	case '\'':
		return "'", 1
	case '"':
		return `"`, 1
	case '?':
		return "?", 1
	case 'x':
		j := i + 1
		hex := ""
		for j < len(r) && len(hex) < 2 && isHex(r[j]) {
			hex += string(r[j])
			j++
		}
		if hex == "" {
			return "x", 1
		}
		var v int
		for _, h := range hex {
			v = v*16 + hexVal(h)
		}
		return string(rune(v)), j - i
	case '0', '1', '2', '3', '4', '5', '6', '7':
		j := i
		oct := ""
		for j < len(r) && len(oct) < 3 && r[j] >= '0' && r[j] <= '7' {
			oct += string(r[j])
			j++
		}
		var v int
		for _, o := range oct {
			v = v*8 + int(o-'0')
		}
		return string(rune(v)), j - i
	}
	return string(c), 1
}

func isHex(c rune) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func hexVal(c rune) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return 0
}

func isVarStart(c rune) bool {
	return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

func isVarCont(c rune) bool {
	return isVarStart(c) || (c >= '0' && c <= '9')
}

// expandVar resolves a small set of variables that affect destructive-command
// classification. IFS expands to a single space (its default whitespace
// behavior) so word splitting still works. Everything else expands to the
// empty string — we are not a real shell.
func expandVar(name string) string {
	if name == "IFS" {
		return " "
	}
	return ""
}
