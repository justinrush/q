package launch

import "bytes"

const codexHookStateHeader = "\n[hooks.state]\n"

// preserveCodexHookState carries Codex's hook approvals into a freshly generated
// q profile. Codex appends [hooks.state] and its child tables to the profile
// after the human approves hooks; q owns everything before that section.
func preserveCodexHookState(generated, existing []byte) []byte {
	stateAt := bytes.Index(existing, []byte(codexHookStateHeader))
	if stateAt < 0 {
		return generated
	}

	var merged []byte
	merged = append(merged, generated...)
	merged = append(merged, existing[stateAt:]...)

	return merged
}
