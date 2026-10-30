package tools

import (
	"fmt"
	"strings"
)

// maxDiffD bounds the Myers search, whose time grows with the file size
// times the number of differences and whose memory grows with the square of
// the differences. Past this many, a diff simply replaces the changed
// region, which is still correct.
const maxDiffD = 1000

// diffOp is one line of an edit script.
type diffOp struct {
	kind byte // ' ' kept, '-' removed, '+' added
	text string
}

// diffLines returns a shortest edit script turning a into b, by Myers'
// O(ND) algorithm ("An O(ND) Difference Algorithm and Its Variations",
// 1986). A common prefix and suffix are split off first, since edits are
// usually small compared to the file.
func diffLines(a, b []string) []diffOp {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	ops := make([]diffOp, 0, len(a)+len(b))
	for _, l := range a[:pre] {
		ops = append(ops, diffOp{' ', l})
	}
	ops = append(ops, myers(a[pre:len(a)-suf], b[pre:len(b)-suf])...)
	for _, l := range a[len(a)-suf:] {
		ops = append(ops, diffOp{' ', l})
	}
	return ops
}

func myers(a, b []string) []diffOp {
	n, m := len(a), len(b)
	if n == 0 || m == 0 {
		return replaceAll(a, b)
	}
	// v[k] is the furthest x reached on diagonal k = x-y. trace[d] keeps the
	// part of v that round d starts from, for diagonals -d..d.
	off := n + m
	v := make([]int, 2*(n+m)+2)
	var trace [][]int
	for d := 0; d <= n+m && d <= maxDiffD; d++ {
		trace = append(trace, append([]int(nil), v[off-d:off+d+1]...))
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || k != d && v[off+k-1] < v[off+k+1] {
				x = v[off+k+1] // down: insert b[y]
			} else {
				x = v[off+k-1] + 1 // right: delete a[x]
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[off+k] = x
			if x >= n && y >= m {
				return backtrack(a, b, trace)
			}
		}
	}
	return replaceAll(a, b)
}

// backtrack walks the trace from the end to recover the edit script.
func backtrack(a, b []string, trace [][]int) []diffOp {
	var rev []diffOp
	x, y := len(a), len(b)
	for d := len(trace) - 1; d >= 0; d-- {
		v := trace[d] // diagonal k is at v[k+d]
		k := x - y
		var prevK int
		if k == -d || k != d && v[k-1+d] < v[k+1+d] {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := 0
		if d > 0 {
			prevX = v[prevK+d]
		}
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			x--
			y--
			rev = append(rev, diffOp{' ', a[x]})
		}
		if d > 0 {
			if x == prevX {
				y--
				rev = append(rev, diffOp{'+', b[y]})
			} else {
				x--
				rev = append(rev, diffOp{'-', a[x]})
			}
		}
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev
}

func replaceAll(a, b []string) []diffOp {
	ops := make([]diffOp, 0, len(a)+len(b))
	for _, l := range a {
		ops = append(ops, diffOp{'-', l})
	}
	for _, l := range b {
		ops = append(ops, diffOp{'+', l})
	}
	return ops
}

// splitLines splits text into lines without their newlines. A final line
// without a newline is marked, so the diff can say so.
func splitLines(text string) (lines []string, noFinalNewline bool) {
	if text == "" {
		return nil, false
	}
	lines = strings.Split(text, "\n")
	if lines[len(lines)-1] == "" {
		return lines[:len(lines)-1], false
	}
	return lines, true
}

// UnifiedDiff returns a unified diff from oldText to newText with three
// lines of context, or "" when they are equal. The names label the two
// sides; carriage returns are left out of the output.
func UnifiedDiff(oldName, newName, oldText, newText string) string {
	if oldText == newText {
		return ""
	}
	a, aNoNL := splitLines(oldText)
	b, bNoNL := splitLines(newText)
	// A changed final newline changes the last line.
	if aNoNL && len(a) > 0 {
		a[len(a)-1] += "\x00"
	}
	if bNoNL && len(b) > 0 {
		b[len(b)-1] += "\x00"
	}
	ops := diffLines(a, b)

	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", oldName, newName)
	const context = 3
	for i := 0; i < len(ops); {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		// A hunk runs from context lines before this change to context lines
		// after the last change that is within 2*context of the previous one.
		start := max(0, i-context)
		end := i
		for j := i; j < len(ops); j++ {
			if ops[j].kind != ' ' {
				end = j + 1
			} else if j-end >= 2*context {
				break
			}
		}
		end = min(len(ops), end+context)
		writeHunk(&out, ops, start, end)
		i = end
	}
	return out.String()
}

func writeHunk(out *strings.Builder, ops []diffOp, start, end int) {
	// Line numbers where the hunk starts on each side.
	oldLine, newLine := 1, 1
	for _, op := range ops[:start] {
		if op.kind != '+' {
			oldLine++
		}
		if op.kind != '-' {
			newLine++
		}
	}
	oldCount, newCount := 0, 0
	for _, op := range ops[start:end] {
		if op.kind != '+' {
			oldCount++
		}
		if op.kind != '-' {
			newCount++
		}
	}
	fmt.Fprintf(out, "@@ -%s +%s @@\n", hunkRange(oldLine, oldCount), hunkRange(newLine, newCount))
	for _, op := range ops[start:end] {
		text, noNL := strings.CutSuffix(op.text, "\x00")
		out.WriteByte(op.kind)
		out.WriteString(strings.TrimSuffix(text, "\r"))
		out.WriteByte('\n')
		if noNL {
			out.WriteString("\\ No newline at end of file\n")
		}
	}
}

// hunkRange formats one side of a hunk header the way git does.
func hunkRange(line, count int) string {
	switch count {
	case 0:
		return fmt.Sprintf("%d,0", line-1)
	case 1:
		return fmt.Sprint(line)
	}
	return fmt.Sprintf("%d,%d", line, count)
}

// DiffStats counts the lines a unified diff adds and removes.
func DiffStats(diff string) (added, removed int) {
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
		case strings.HasPrefix(line, "+"):
			added++
		case strings.HasPrefix(line, "-"):
			removed++
		}
	}
	return added, removed
}
