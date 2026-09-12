package mission

import (
	"strings"
	"testing"
)

func TestComposePromptBranchRoles(t *testing.T) {
	ms := Mission{
		Name:         "Fix login",
		Prompt:       "Update the login branch.",
		BaseBranches: map[string]string{"app": " feat/login ", "docs": "  ", "failed": "feat/failed"},
		Work: map[string]RepoWork{
			"app":    {RepoName: "app", Branch: "jane/fix-login", BaseRef: "refs/remotes/origin/feat/login", BaseSHA: "1234567890", Created: true},
			"docs":   {RepoName: "docs", Branch: "jane/fix-login", BaseRef: "refs/remotes/origin/main", BaseSHA: "abcdef1234", Created: true},
			"failed": {RepoName: "failed", Created: false},
		},
	}

	prompt, err := ComposePrompt(Operation{Name: "Development"}, ms)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"branch jane/fix-login, from origin/feat/login at 1234567",
		"User-selected existing branch: feat/login on origin. jane/fix-login is this mission's isolated working branch.",
		"branch jane/fix-login, from origin/main at abcdef1",
		"A base branch is the starting point, not by itself an instruction to publish.",
		"git push origin HEAD:refs/heads/<existing-branch>",
		"do not force-push over other missions' work",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if got := strings.Count(prompt, "User-selected existing branch:"); got != 1 {
		t.Errorf("selected branch annotations = %d, want 1", got)
	}
	if strings.Contains(prompt, "feat/failed") {
		t.Error("prompt includes an unprovisioned repository")
	}
}

func TestComposePromptWithoutRepositories(t *testing.T) {
	prompt, err := ComposePrompt(Operation{Name: "Development"}, Mission{Name: "Plan", Prompt: "Make a plan."})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt, "git push") || strings.Contains(prompt, "## Repositories") {
		t.Error("repo-less mission includes repository instructions")
	}
	if !strings.Contains(prompt, "Make a plan.") {
		t.Error("prompt missing mission instructions")
	}
}
