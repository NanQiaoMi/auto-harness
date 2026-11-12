package tools

import "strings"

// diffCellLimit bounds the LCS table used by DiffLines. A larger pair
// of blocks is reported as a wholesale replacement instead of a
// line-by-line diff, so a huge file cannot blow up the renderer.
const diffCellLimit = 1 << 20

// diffKind is the kind of a diff operation.
type diffKind int

const (
	diffEqual diffKind = iota
	diffDelete
	diffInsert
)

// diffOp is one line of a diff.
type diffOp struct {
	kind diffKind
	text string
}

// DiffLines returns the lines that differ between two texts, in a
// unified-diff-like form: " " marks an unchanged line, "-" a line
// that was removed and "+" a line that was added.
//
// Only the lines that differ are reported: the common prefix and
// suffix are dropped first, so an edit inside a large file stays
// cheap.
func DiffLines(before, after string) []string {
	b := splitDiffLines(before)
	a := splitDiffLines(after)

	prefix := 0
	for prefix < len(b) && prefix < len(a) && b[prefix] == a[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(b)-prefix && suffix < len(a)-prefix &&
		b[len(b)-1-suffix] == a[len(a)-1-suffix] {
		suffix++
	}

	ops := diffOps(b[prefix:len(b)-suffix], a[prefix:len(a)-suffix])
	out := make([]string, 0, len(ops))
	for _, op := range ops {
		switch op.kind {
		case diffDelete:
			out = append(out, "-"+op.text)
		case diffInsert:
			out = append(out, "+"+op.text)
		default:
			out = append(out, " "+op.text)
		}
	}
	return out
}

// splitDiffLines splits text into lines, dropping the trailing empty
// line a final newline would produce.
func splitDiffLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// diffOps computes the edit script turning b into a.
func diffOps(b, a []string) []diffOp {
	if len(b) == 0 && len(a) == 0 {
		return nil
	}
	if len(b) == 0 {
		return insertOps(a)
	}
	if len(a) == 0 {
		return deleteOps(b)
	}
	if len(b)*len(a) > diffCellLimit {
		// Too big to align: report the whole replacement.
		return append(deleteOps(b), insertOps(a)...)
	}

	// The classic longest-common-subsequence table, filled from the
	// end so the walk below can read it forwards.
	n, m := len(b), len(a)
	table := make([][]int, n+1)
	for i := range table {
		table[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case b[i] == a[j]:
				table[i][j] = table[i+1][j+1] + 1
			case table[i+1][j] >= table[i][j+1]:
				table[i][j] = table[i+1][j]
			default:
				table[i][j] = table[i][j+1]
			}
		}
	}

	var ops []diffOp
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case b[i] == a[j]:
			ops = append(ops, diffOp{diffEqual, b[i]})
			i++
			j++
		case table[i+1][j] >= table[i][j+1]:
			ops = append(ops, diffOp{diffDelete, b[i]})
			i++
		default:
			ops = append(ops, diffOp{diffInsert, a[j]})
			j++
		}
	}
	ops = append(ops, deleteOps(b[i:])...)
	ops = append(ops, insertOps(a[j:])...)
	return ops
}

// deleteOps turns every line into a deletion.
func deleteOps(lines []string) []diffOp {
	if len(lines) == 0 {
		return nil
	}
	ops := make([]diffOp, 0, len(lines))
	for _, l := range lines {
		ops = append(ops, diffOp{diffDelete, l})
	}
	return ops
}

// insertOps turns every line into an insertion.
func insertOps(lines []string) []diffOp {
	if len(lines) == 0 {
		return nil
	}
	ops := make([]diffOp, 0, len(lines))
	for _, l := range lines {
		ops = append(ops, diffOp{diffInsert, l})
	}
	return ops
}
