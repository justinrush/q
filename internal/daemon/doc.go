// Package daemon is the long-lived process that owns q's state.
//
// Everything else is a client. The TUI, the CLI subcommands, and the agent hook
// bridge all mutate state by calling this server, which is what makes a plain
// JSON state file safe: there is exactly one writer.
//
// The daemon exists rather than hosting the server inside the TUI because agents
// outlive the TUI. A mission keeps running after the board is closed, its hooks
// still need somewhere to report, and the reconciler that keeps cards honest has
// to run when nobody is watching.
package daemon
