package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// absPath expands a leading ~ and resolves a path to an absolute one.
//
// Repo paths are stored absolute because the daemon resolves them long after the
// shell that supplied them has exited, and from a different working directory.
func absPath(p string) (string, error) {
	if after, ok := strings.CutPrefix(p, "~"); ok && (after == "" || strings.HasPrefix(after, "/")) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expanding %q: %w", p, err)
		}

		p = filepath.Join(home, after)
	}

	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("resolving %q: %w", p, err)
	}

	return abs, nil
}

// baseName returns the final path element, which is how q names a repo.
func baseName(p string) string {
	return filepath.Base(filepath.Clean(p))
}
