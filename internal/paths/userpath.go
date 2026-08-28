// User-supplied path handling, shared by the CLI's repo arguments and the
// board's repo picker.

package paths

import (
	"os"
	"path/filepath"
	"strings"
)

// Expand resolves a path the user typed, returning "" when the text is not a path.
//
// A bare name is not treated as a relative path: the TUI's working directory is
// whatever shell started it, which the user cannot see, so resolving "weave"
// against it would silently mean something different from one session to the next.
func Expand(p string) string {
	p = strings.TrimSpace(p)

	if after, ok := strings.CutPrefix(p, "~"); ok && (after == "" || strings.HasPrefix(after, "/")) {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}

		return filepath.Join(home, after)
	}

	if filepath.IsAbs(p) || strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../") || p == "." {
		abs, err := filepath.Abs(p)
		if err != nil {
			return ""
		}

		return abs
	}

	return ""
}

// Dir reports whether typed text is already a directory, returning it absolute.
// Pasting a full path has to keep working.
func Dir(p string) (string, bool) {
	abs := Expand(p)
	if abs == "" {
		return "", false
	}

	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return "", false
	}

	return abs, true
}

// Display collapses the home directory to ~, so a picker shows the short form of
// a path the user recognizes.
func Display(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}

	if rel, ok := strings.CutPrefix(path, home); ok && (rel == "" || strings.HasPrefix(rel, "/")) {
		return "~" + rel
	}

	return path
}
