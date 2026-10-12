package mission

import (
	"fmt"
	"strings"
	"text/template"
)

// DefaultReviewFocus is a review's brief when the human names nothing in
// particular to look at.
const DefaultReviewFocus = "Review the whole mission and report what I should know before accepting its work."

// reviewTemplate is the prompt a reviewing agent receives.
//
// It asks for two things a diff alone does not give: an account of what the
// first agent decided and why, and a list of what it claimed without showing.
// Those are the parts of a finished mission a human otherwise has to
// reconstruct by reading the whole conversation.
var reviewTemplate = template.Must(template.New("review").Parse(
	`# Review of mission: {{.SourceName}}

Another coding agent ({{.SourceAgent}}) worked on the mission below. You are a
second, independent reviewer. The human who ordered the work will read your report
before deciding whether to accept it, so your job is to tell them what they would
want to know and could not see from the agent's own summary.

You are reviewing, not fixing. Do not edit, commit, stash, reset, or push anything
in the repositories under review. Reading, building, and running tests there is
fine. Write only inside your own working directory.

## Operation: {{.OperationName}}
{{if .Summary}}
{{.Summary}}
{{end}}
## The mission as it was briefed

{{.SourceBrief}}

## The work
{{if .Repos}}
Each repository below is the git worktree that agent worked in. It may hold
uncommitted changes, which are part of the work.
{{range .Repos}}
- {{.Name}}: {{.Path}} (branch {{.Branch}}, started from {{.BaseRef}} at {{.ShortSHA}})
  - commits: git -C {{.Path}} log --stat {{.BaseSHA}}..HEAD
  - everything changed, committed or not: git -C {{.Path}} diff {{.BaseSHA}}
  - uncommitted and untracked files: git -C {{.Path}} status --short
{{- end}}
{{else}}
That mission had no repositories. Review what the agent reported instead.
{{end}}{{if .LastMessage}}
The opening of that agent's closing message:

{{.LastMessage}}
{{end}}
## How that agent got there
{{if .Transcript}}
A copy of that agent's own conversation log is at {{.Transcript}}. It is
JSON Lines and can be very large, with whole file contents embedded in it, so search
it rather than reading it end to end. It is where the decisions are: what the agent
considered and rejected, what it tried that failed, what it was told by the human
along the way, and what it chose not to do. Its format belongs to that agent and
is not documented here; inspect a few lines to learn its shape first.
{{else}}
No conversation log from that agent is available on this machine. Work from the
brief, the changes, and the commit messages, and say in your report that the
agent's reasoning could not be examined.
{{end}}
## What the human asked you to look at

{{.Focus}}

## Your report

Write the report to {{.ReportPath}} as Markdown, with these sections. Leave a
section out only if you looked and found nothing for it.

1. **Verdict**: one or two sentences. Does the work do what was briefed?
2. **What changed**: per repository, the substance of the change, not a file list.
3. **Decisions and trade-offs**: the choices the agent made that the human did not
   explicitly ask for, why it made them, and the alternatives it considered or
   abandoned. Quote the agent where its reasoning is on record, and say so plainly
   where you are inferring.
4. **Departures from the brief**: anything asked for and not done, done differently,
   or done without being asked.
5. **Risks and defects**: bugs, regressions, missing tests, and fragile assumptions,
   most serious first, each with a file and line.
6. **Unverified claims**: things the agent said were true (tests pass, behavior
   checked) that you could not confirm, or confirmed to be false.
7. **Questions for the human**: what they need to decide.

Be specific and brief. Report what you checked, not what you assume. Do not praise.

When the report is written, end your turn with a message whose first line is the
verdict as a single sentence, followed by the three findings that matter most and
the path to the full report. Phrase it as statements, not as a question.
`))

// reviewData is the review template's view of a review.
type reviewData struct {
	OperationName string
	Summary       string
	SourceName    string
	SourceAgent   string
	SourceBrief   string
	LastMessage   string
	Transcript    string
	Focus         string
	ReportPath    string
	Repos         []reviewRepo
}

// reviewRepo describes one of the reviewed mission's worktrees.
type reviewRepo struct {
	Name     string
	Path     string
	Branch   string
	BaseRef  string
	BaseSHA  string
	ShortSHA string
}

// ComposeReviewPrompt builds the prompt for a mission that reviews source.
//
// transcript is where a copy of the reviewed agent's conversation log was put,
// or empty when there is none to offer.
//
// Like [ComposePrompt] it is pure, and for the same reason: this text is the
// whole of what the reviewer knows about the work, so it has to be assertable.
// It is composed at launch rather than stored on the mission because every path
// in it is true on one host only.
func ComposeReviewPrompt(operation Operation, source, review Mission, transcript string) (string, error) {
	focus := strings.TrimSpace(review.Prompt)
	if focus == "" {
		focus = DefaultReviewFocus
	}

	data := reviewData{
		OperationName: operation.Name,
		Summary:       strings.TrimSpace(operation.Summary),
		SourceName:    source.Name,
		SourceAgent:   describeAgent(source),
		SourceBrief:   quoteBlock(source.Prompt),
		LastMessage:   quoteBlock(source.LastMessage),
		Transcript:    transcript,
		Focus:         focus,
		ReportPath:    ArtifactDir + "/" + ReviewFile,
	}

	for _, work := range source.Worktrees() {
		if !work.Created {
			continue
		}

		data.Repos = append(data.Repos, reviewRepo{
			Name:     work.RepoName,
			Path:     work.WorktreePath,
			Branch:   work.Branch,
			BaseRef:  shortRef(work.BaseRef),
			BaseSHA:  work.BaseSHA,
			ShortSHA: shortSHA(work.BaseSHA),
		})
	}

	var b strings.Builder
	if err := reviewTemplate.Execute(&b, data); err != nil {
		return "", fmt.Errorf("composing the review prompt for mission %s: %w", review.ID, err)
	}

	return b.String(), nil
}

// describeAgent names the agent a mission ran on, with its model and effort
// when the mission chose them.
func describeAgent(ms Mission) string {
	parts := []string{string(ms.Tool)}

	if ms.Model != "" {
		parts = append(parts, ms.Model)
	}

	if ms.Effort != "" {
		parts = append(parts, ms.Effort+" effort")
	}

	return strings.Join(parts, ", ")
}

// quoteBlock renders text as a Markdown quotation.
//
// The reviewed brief and message are another party's words, and each can carry
// headings and instructions of its own. Quoting keeps them visibly inside the
// section that introduces them instead of reading as part of the reviewer's
// own orders.
func quoteBlock(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}

	var b strings.Builder

	for line := range strings.SplitSeq(text, "\n") {
		b.WriteString(strings.TrimRight("> "+line, " "))
		b.WriteString("\n")
	}

	return strings.TrimRight(b.String(), "\n")
}
