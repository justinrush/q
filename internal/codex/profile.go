package codex

import "bytes"

// hookStateHeader introduces the section codex owns.
const hookStateHeader = "\n[hooks.state]\n"

// mergeProfile carries Codex's hook approvals into a freshly generated q
// profile, and reports whether the result differs from what is on disk.
//
// Codex appends [hooks.state] and its child tables to the profile after the
// human approves hooks; q owns everything before that section.
func mergeProfile(generated, existing []byte) ([]byte, bool) {
	merged := generated

	if stateAt := bytes.Index(existing, []byte(hookStateHeader)); stateAt >= 0 {
		merged = append(append([]byte{}, generated...), existing[stateAt:]...)
	}

	return merged, !bytes.Equal(merged, existing)
}
