// Package power tells a machine that is awake from one that only looks it.
//
// A Mac with its lid closed is not asleep the whole time. It wakes for a few
// seconds at a time, with the display off, to do maintenance and network work —
// a dark wake — and every background process runs during those seconds,
// including q's daemon. To the paired q, a primary that exchanges during one is
// indistinguishable from a laptop that has been opened, so it hands missions
// back to a machine that is asleep again a moment later.
//
// macOS reports which subsystems are up. During a dark wake the graphics
// subsystem is not; during a full wake it is, even with the display asleep.
package power

import (
	"context"
	"runtime"
	"strings"
	"time"

	"github.com/justinrush/q/internal/runner"
)

// pmset is where macOS keeps its power tool.
const pmset = "/usr/bin/pmset"

// probeTimeout bounds one question to pmset, which answers in milliseconds.
const probeTimeout = 5 * time.Second

// Probe returns a function reporting whether this machine is fully awake, or
// nil on a platform with no dark wake to tell apart.
//
// Any failure to find out reads as awake. Not knowing is how q behaved before
// it asked, and a machine that wrongly believed itself asleep would stop
// exchanging with its peer altogether.
func Probe(run runner.Runner) func(context.Context) bool {
	if runtime.GOOS != "darwin" {
		return nil
	}

	return func(ctx context.Context) bool {
		res, err := run.Run(ctx, runner.Spec{
			Name:    pmset,
			Args:    []string{"-g", "systemstate"},
			Timeout: probeTimeout,
		})
		if err != nil {
			return true
		}

		return FullyAwake(string(res.Stdout))
	}
}

// FullyAwake reads the output of `pmset -g systemstate`, which includes a line
// such as
//
//	Current System Capabilities are: CPU Graphics Audio Network
//
// and reports false only when that line is there and Graphics is not in it.
func FullyAwake(systemstate string) bool {
	const prefix = "Current System Capabilities are:"

	for line := range strings.Lines(systemstate) {
		caps, ok := strings.CutPrefix(strings.TrimSpace(line), prefix)
		if !ok {
			continue
		}

		for _, c := range strings.Fields(caps) {
			if c == "Graphics" {
				return true
			}
		}

		return false
	}

	return true
}
