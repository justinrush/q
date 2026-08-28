package main

import (
	"context"
	"io"
	"log/slog"
	"os"
)

// newLogger builds the logger for this process.
//
// Output is logfmt rather than JSON because these logs are read by a person far
// more often than by a program: q writes them to a file under the state
// directory, and the first thing anyone does with that file is grep it.
func newLogger(w io.Writer, level string) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: parseLevel(level)}))
}

// parseLevel converts a configured level name to a slog level.
//
// An unrecognized name resolves to info rather than failing: a typo in a config
// file should not silence a daemon, and slog's own parser accepts the spellings
// people actually write ("warn", "WARN", "debug").
func parseLevel(name string) slog.Level {
	var level slog.Level
	if err := level.UnmarshalText([]byte(name)); err != nil {
		return slog.LevelInfo
	}

	return level
}

// ctxKey is the unexported context key carrying the logger.
//
// slog deliberately provides no logger-in-context helper, and the alternative —
// slog.SetDefault — is process-global state that a subcommand could not scope.
// The TUI needs exactly that scoping: it redirects logging to a file for as long
// as the alternate screen is up.
type ctxKey struct{}

// withLogger returns a context carrying the logger.
func withLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// loggerFrom returns the context's logger, falling back to the default.
func loggerFrom(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok && l != nil {
		return l
	}

	return slog.Default()
}

// bootstrapLogger is the logger in force until the config file is loaded. Only
// the environment can influence it, because nothing else has been read yet.
func bootstrapLogger() *slog.Logger {
	return newLogger(os.Stderr, os.Getenv(EnvLogLevel))
}
