package tools

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// matchGlob reports whether name matches pattern. Both use '/' between
// path segments. In a segment, '*' matches any run of characters, '?' any
// one character, and '[...]' a character class ('!' or '^' negates, '-'
// makes a range); a backslash escapes the next character. A whole segment
// "**" matches any number of segments, including none, except at the end
// of the pattern, where it matches at least one. When fold is set, letter
// case is ignored.
func matchGlob(pattern, name string, fold bool) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"), fold)
}

func matchSegments(pat, name []string, fold bool) bool {
	for len(pat) > 0 {
		if pat[0] != "**" {
			if len(name) == 0 || !matchSegment(pat[0], name[0], fold) {
				return false
			}
			pat, name = pat[1:], name[1:]
			continue
		}
		for len(pat) > 1 && pat[1] == "**" {
			pat = pat[1:]
		}
		if len(pat) == 1 {
			return len(name) > 0
		}
		for i := 0; i <= len(name); i++ {
			if matchSegments(pat[1:], name[i:], fold) {
				return true
			}
		}
		return false
	}
	return len(name) == 0
}

// matchSegment matches one path segment against one pattern segment.
func matchSegment(pat, s string, fold bool) bool {
	// Classic wildcard matching: on a mismatch, backtrack to the last '*'
	// and let it swallow one more character.
	starPat, starS := -1, 0
	p, i := 0, 0
	for i < len(s) {
		if p < len(pat) {
			switch pat[p] {
			case '*':
				starPat, starS = p, i
				p++
				continue
			case '?':
				_, n := utf8.DecodeRuneInString(s[i:])
				p, i = p+1, i+n
				continue
			case '[':
				r, n := utf8.DecodeRuneInString(s[i:])
				end, in := matchClass(pat[p:], r, fold)
				if end > 0 && in {
					p, i = p+end, i+n
					continue
				}
				if end == 0 && r == '[' { // malformed: a literal bracket
					p, i = p+1, i+1
					continue
				}
			default:
				pr, pn := utf8.DecodeRuneInString(pat[p:])
				if pr == '\\' && p+1 < len(pat) {
					pr, pn = utf8.DecodeRuneInString(pat[p+1:])
					pn++
				}
				r, n := utf8.DecodeRuneInString(s[i:])
				if sameRune(pr, r, fold) {
					p, i = p+pn, i+n
					continue
				}
			}
		}
		if starPat < 0 {
			return false
		}
		_, n := utf8.DecodeRuneInString(s[starS:])
		starS += n
		p, i = starPat+1, starS
	}
	for p < len(pat) && pat[p] == '*' {
		p++
	}
	return p == len(pat)
}

// matchClass matches r against the character class at the start of pat.
// It returns the length of the class and whether r is in it. A malformed
// class, without a closing bracket, has length 0: its '[' is literal.
func matchClass(pat string, r rune, fold bool) (length int, in bool) {
	i := 1
	negate := false
	if i < len(pat) && (pat[i] == '!' || pat[i] == '^') {
		negate = true
		i++
	}
	matched := false
	for first := true; ; first = false {
		if i >= len(pat) {
			return 0, false // no closing bracket
		}
		if pat[i] == ']' && !first {
			break
		}
		lo, n := classRune(pat[i:])
		i += n
		hi := lo
		if i+1 < len(pat) && pat[i] == '-' && pat[i+1] != ']' {
			hi, n = classRune(pat[i+1:])
			i += 1 + n
		}
		if inRange(r, lo, hi) || fold && (inRange(unicode.ToLower(r), lo, hi) || inRange(unicode.ToUpper(r), lo, hi)) {
			matched = true
		}
	}
	return i + 1, matched != negate
}

func classRune(s string) (rune, int) {
	if s[0] == '\\' && len(s) > 1 {
		r, n := utf8.DecodeRuneInString(s[1:])
		return r, n + 1
	}
	return utf8.DecodeRuneInString(s)
}

func inRange(r, lo, hi rune) bool { return lo <= r && r <= hi }

func sameRune(a, b rune, fold bool) bool {
	return a == b || fold && unicode.ToLower(a) == unicode.ToLower(b)
}

// hasGlobMeta reports whether a pattern contains wildcards.
func hasGlobMeta(s string) bool { return strings.ContainsAny(s, `*?[\`) }
