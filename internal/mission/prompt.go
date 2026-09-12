package mission

import (
	"fmt"
	"strings"
	"text/template"
)

// promptTemplate is the prompt every mission's agent receives.
//
// The repo section is the point of the whole exercise: the agent is told which
// worktrees exist, what branch they are on, and that they contain tracked files
// only, so it does not waste a turn discovering that node_modules is missing or,
// worse, assume the checkout is broken.
var promptTemplate = template.Must(template.New("prompt").Parse(
	`# Operation: {{.OperationName}}
{{if .Summary}}
{{.Summary}}
{{end}}{{if .Repos}}
## Repositories

Your working directory is the mission root. Each repository below is a git worktree
of the user's own checkout, on a separate branch created for this mission. They contain
tracked files only, so run any install or initialization step yourself if you
need one.
{{range .Repos}}
- {{.Name}}: ./{{.Name}} (branch {{.Branch}}, from {{.BaseRef}} at {{.ShortSHA}})
{{- if .SelectedBase}}
  User-selected existing branch: {{.SelectedBase}} on origin. {{.Branch}} is this mission's isolated working branch.
{{- end}}
{{- end}}

Commit on the mission branches listed above; keep other checkouts untouched.
A base branch is the starting point, not by itself an instruction to publish.
When the task is to update a user-selected existing branch, that existing branch
is the destination for an authorized push or the base for a requested PR/MR.
For that workflow, do not substitute the mission branch as the push destination or open a PR/MR
against the repository's default branch unless the user asks for that workflow.
Before pushing to an existing branch, fetch its latest state, integrate any
concurrent changes into the mission branch, and rerun relevant checks. Push with
an explicit destination (git push origin HEAD:refs/heads/<existing-branch>).
If the push is rejected because the branch advanced, fetch and integrate again;
do not force-push over other missions' work. Report which branch received the work.
{{end}}
## Mission: {{.MissionName}}

{{.Prompt}}
`))

// promptData is the template's view of a
type promptData struct {
	OperationName string
	Summary       string
	MissionName   string
	Prompt        string
	Repos         []promptRepo
}

// promptRepo describes one worktree to the agent.
type promptRepo struct {
	Name         string
	Branch       string
	BaseRef      string
	ShortSHA     string
	SelectedBase string
}

// ComposePrompt builds the prompt for a
//
// It is a pure function so that the exact text handed to an agent can be asserted
// against a golden file; the prompt is the primary interface between q and
// the agent, and a silent change to it changes every mission's behavior.
func ComposePrompt(operation Operation, ms Mission) (string, error) {
	data := promptData{
		OperationName: operation.Name,
		Summary:       strings.TrimSpace(operation.Summary),
		MissionName:   ms.Name,
		Prompt:        strings.TrimSpace(ms.Prompt),
	}

	for _, work := range ms.Worktrees() {
		if !work.Created {
			continue
		}

		data.Repos = append(data.Repos, promptRepo{
			Name:         work.RepoName,
			Branch:       work.Branch,
			BaseRef:      shortRef(work.BaseRef),
			ShortSHA:     shortSHA(work.BaseSHA),
			SelectedBase: strings.TrimSpace(ms.BaseBranches[work.RepoName]),
		})
	}

	var b strings.Builder
	if err := promptTemplate.Execute(&b, data); err != nil {
		return "", fmt.Errorf("composing prompt for mission %s: %w", ms.ID, err)
	}

	return b.String(), nil
}

// shortRef trims the refs/remotes prefix so the prompt reads as "origin/main".
func shortRef(ref string) string {
	if trimmed, ok := strings.CutPrefix(ref, "refs/remotes/"); ok {
		return trimmed
	}

	return ref
}

// shortSHA abbreviates a commit to seven characters.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}

	return sha
}
