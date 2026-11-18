package terminal

import "strings"

// ShellJoin renders argv as one command line a POSIX shell splits back into
// the same arguments.
//
// Ghostty runs a window's command through the login shell, and a command line
// shown to the user is pasted into one, so each argument a shell would
// rewrite is single-quoted. That includes a leading ~: the attach command
// names the peer's q as ~/.local/bin/q for the far end's shell to expand
// against its own home, and a local shell expanding it first sends this
// machine's home directory across instead.
//
// Arguments made only of characters no shell treats specially are left bare,
// so the common case stays readable.
func ShellJoin(argv []string) string {
	out := make([]string, len(argv))
	for i, arg := range argv {
		out[i] = shellQuote(arg)
	}

	return strings.Join(out, " ")
}

// shellQuote single-quotes s unless every character in it is safe bare.
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, unsafeInShell) < 0 {
		return s
	}

	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// unsafeInShell reports whether a shell could treat r as anything but itself.
func unsafeInShell(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return false
	}

	return !strings.ContainsRune("-_./=:,+@%", r)
}
