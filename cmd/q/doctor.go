package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"

	"github.com/justinrush/q/internal/paths"
	"github.com/justinrush/q/internal/runner"
	"github.com/spf13/cobra"
)

// versionFlag is what most tools take to report their version.
const versionFlag = "--version"

// versionArgs are the arguments used to ask each tool its version. Tools absent
// from this map are only checked for presence.
var versionArgs = map[toolName][]string{
	toolGit:    {versionFlag},
	toolTmux:   {"-V"},
	toolClaude: {versionFlag},
	toolCodex:  {versionFlag},
	toolGemini: {versionFlag},
}

// envWarnings are environment variables that silently degrade q. Each is
// reported when set, with an explanation of what it affects.
var envWarnings = map[string]string{
	"CLAUDE_CODE_DISABLE_AGENT_VIEW":                     "disables `claude agents --json`; q does not rely on it, but it is a useful debugging tool",
	"CLAUDE_CODE_DISABLE_PERMISSION_PROMPT_NOTIFY_HOOKS": "suppresses claude's permission_prompt Notification hook; q's primary signal is PermissionRequest, so this is tolerable",
}

func buildDoctorSubcommand() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check that q can find everything it needs",
		Long: "Report the resolved paths and versions of the external tools q drives, " +
			"the directories it writes to, and any environment settings that would degrade it.",
		Args: cobra.NoArgs,
		RunE: runDoctor,
	}
}

func runDoctor(cmd *cobra.Command, _ []string) error {
	rep := newReport()

	reportSelf(rep)
	reportConfig(rep)
	reportTools(cmd.Context(), rep)

	dirs, err := paths.Resolve(pathOverrides())
	if err != nil {
		return err
	}

	if err := reportDirs(rep, dirs); err != nil {
		return err
	}

	reportOrphans(cmd.Context(), rep, dirs)
	reportEnv(rep)

	_, writeErr := io.WriteString(cmd.OutOrStdout(), rep.String())

	return writeErr
}

// report accumulates the doctor output so it can be emitted with a single
// checked write. Two kinds of content are interleaved: plain lines, and
// tab-aligned rows that align as a block within each run of consecutive rows.
type report struct {
	b  strings.Builder
	tw *tabwriter.Writer
}

func newReport() *report {
	r := &report{}
	r.tw = tabwriter.NewWriter(&r.b, 0, 0, 2, ' ', 0)

	return r
}

// line appends a plain line, first flushing any pending aligned rows so output
// order is preserved.
//
// Writes to a strings.Builder cannot fail, which is why the errors from Fprintf
// and Flush are discarded here rather than propagated.
func (r *report) line(format string, args ...any) {
	_ = r.tw.Flush()
	_, _ = fmt.Fprintf(&r.b, format+"\n", args...)
}

// row appends a tab-aligned row. Use \t to separate columns.
func (r *report) row(format string, args ...any) {
	_, _ = fmt.Fprintf(r.tw, format+"\n", args...)
}

// String returns the accumulated report.
func (r *report) String() string {
	_ = r.tw.Flush()

	return r.b.String()
}

// reportSelf prints the path of the running binary. Hook commands embed this
// path in generated agent configuration, so a moved binary silently breaks
// status reporting for already-running sessions.
func reportSelf(rep *report) {
	self, err := os.Executable()
	if err != nil {
		self = fmt.Sprintf("unknown (%v)", err)
	}

	rep.line("q")
	rep.row("  binary\t%s", self)

	if tmux := os.Getenv("TMUX"); tmux != "" {
		rep.row("  tmux\trunning inside tmux (%s)", strings.SplitN(tmux, ",", 2)[0])
	} else {
		rep.row("  tmux\tnot inside tmux; a debrief opens its own window")
	}

	rep.line("")
}

// reportConfig prints where the configuration came from and the settings most
// likely to be pointing q at the wrong place.
func reportConfig(rep *report) {
	path := configPath()

	rep.line("config")
	rep.row("  file\t%s\t%s", path, describe(path))
	rep.row("  repo roots\t%s", strings.Join(cfg.Repos.Roots, ", "))
	rep.row("  branch prefix\t%s", branchPrefixOrUser(cfg))
	rep.row("  default agent\t%s", cfg.Agents.Default)
	rep.row("  editor\t%s", strings.Join(cfg.Editor.Command, " "))
	rep.row("  terminal\t%s\t%s", cfg.Terminal.Mode, strings.Join(cfg.Terminal.Command, " "))
	rep.line("")
}

// reportEditor resolves the configured editor the way a debrief pane will.
func reportEditor(rep *report) {
	command := cfg.Editor.Command
	if len(command) == 0 {
		command = defaultEditorCommand()
	}

	path, err := exec.LookPath(command[0])
	if err != nil {
		rep.row("  editor\tMISSING\t%v", err)

		return
	}

	rep.row("  editor\t%s\t%s", path, strings.Join(command[1:], " "))
}

// reportTools prints each tool's resolved path and version, or why it could not
// be found.
func reportTools(ctx context.Context, rep *report) {
	rep.line("tools")

	res := newToolResolver(toolOptionsFor(cfg))
	run := runner.OS{}

	for _, tool := range allTools {
		path, err := res.Path(tool)
		if err != nil {
			rep.row("  %s\tMISSING\t%v", tool, err)

			continue
		}

		rep.row("  %s\t%s\t%s", tool, path, toolVersion(ctx, run, tool, path))

		if tool == toolCodex {
			reportCodexAppServer(ctx, rep, run, path)
		}
	}

	reportEditor(rep)

	if cfg.Terminal.Mode == terminalGhostty {
		reportGhostty(rep)
	}

	rep.line("")
}

// reportCodexAppServer verifies the two subcommands Q needs for structured
// Codex status. The help probe is read-only and does not start the managed server.
func reportCodexAppServer(ctx context.Context, rep *report, run runner.Runner, path string) {
	_, err := run.Run(ctx, runner.Spec{
		Name: path,
		Args: []string{"app-server", "proxy", "--help"},
	})
	if err != nil {
		rep.row("  codex app-server\tUNAVAILABLE\t%v", err)

		return
	}

	rep.row("  codex app-server\tavailable\tremote TUI + status proxy")
}

// toolVersion asks a tool its version, returning a short single-line answer.
func toolVersion(ctx context.Context, run runner.Runner, tool toolName, path string) string {
	args, ok := versionArgs[tool]
	if !ok {
		return ""
	}

	res, err := run.Run(ctx, runner.Spec{Name: path, Args: args})
	if err != nil && !runner.IsExit(err) {
		return fmt.Sprintf("(version check failed: %v)", err)
	}

	line := res.Out()
	if idx := strings.IndexByte(line, '\n'); idx >= 0 {
		line = line[:idx]
	}

	return line
}

// reportGhostty checks for the app bundle q controls in Ghostty terminal mode.
func reportGhostty(rep *report) {
	if fi, err := os.Stat(ghosttyApp); err == nil && fi.IsDir() {
		rep.row("  ghostty\t%s\t1.3+ required", ghosttyApp)

		return
	}

	rep.row("  ghostty\tMISSING\t%s not found; install Ghostty 1.3 or newer",
		ghosttyApp)
}

// reportDirs prints where q stores its data, creating anything absent.
func reportDirs(rep *report, dirs paths.Dirs) error {
	if err := dirs.Ensure(); err != nil {
		return err
	}

	rep.line("directories")

	for _, row := range [][2]string{
		{"data", dirs.Data},
		{"missions", dirs.MissionsDir()},
		{"state", dirs.State},
		{"logs", dirs.LogsDir()},
	} {
		rep.row("  %s\t%s\t%s", row[0], row[1], describe(row[1]))
	}

	rep.line("")

	return nil
}

// describe reports whether a path exists and, if so, its permission bits.
func describe(p string) string {
	fi, err := os.Stat(p)
	if os.IsNotExist(err) {
		return "absent"
	}

	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}

	return fi.Mode().Perm().String()
}

// reportEnv prints environment settings that would degrade q.
func reportEnv(rep *report) {
	rep.line("environment")

	var found bool

	for name, effect := range envWarnings {
		value := os.Getenv(name)
		if value == "" {
			continue
		}

		found = true

		rep.line("  %s=%s", name, value)
		rep.line("    %s", effect)
	}

	if !found {
		rep.line("  no degrading environment variables set")
	}
}
