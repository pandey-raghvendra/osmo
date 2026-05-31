// Package diff renders unified text diffs for proposed file changes.
package diff

import (
	"strings"

	"github.com/pmezard/go-difflib/difflib"
)

// Unified returns a unified diff between before and after for the given path.
func Unified(path string, before, after []byte) string {
	ud := difflib.UnifiedDiff{
		A:        difflib.SplitLines(string(before)),
		B:        difflib.SplitLines(string(after)),
		FromFile: "a/" + path,
		ToFile:   "b/" + path,
		Context:  3,
	}
	out, err := difflib.GetUnifiedDiffString(ud)
	if err != nil {
		return "<diff error: " + err.Error() + ">\n"
	}
	if strings.TrimSpace(out) == "" {
		return "(no textual change)\n"
	}
	return out
}
