package codex

import (
	"context"

	"github.com/justinrush/q/internal/mission"
)

// threadReader is the part of the app-server client [Runtime] needs. It is
// declared here so a test can supply readings without a live codex process.
type threadReader interface {
	ReadThread(ctx context.Context, threadID string) (ThreadSnapshot, error)
	FindThread(ctx context.Context, cwd string) (ThreadSnapshot, bool, error)
	Close() error
}

// Runtime reads process truth from codex's app-server.
//
// It implements [mission.Runtime]. Unlike a session registry, app-server reports
// what the running thread is actually doing, so q polls it and lets its readings
// move a card in either direction. Hooks still supply tool names and
// closing-message text.
type Runtime struct {
	threads threadReader
}

// NewRuntime returns a runtime reading through the given app-server client.
func NewRuntime(threads threadReader) *Runtime { return &Runtime{threads: threads} }

// Read reports what codex says the mission's thread is doing.
//
// A mission whose session id is not yet known is located by its working
// directory instead, which is unique per mission.
func (r *Runtime) Read(ctx context.Context, ms mission.Mission) (mission.Reading, bool, error) {
	if ms.Tool != mission.ToolCodex {
		return mission.Reading{}, false, nil
	}

	if ms.AgentSessionID != "" {
		thread, err := r.threads.ReadThread(ctx, ms.AgentSessionID)
		if err != nil {
			return mission.Reading{}, false, err
		}

		if thread.Status.Type != statusNotLoaded {
			return r.reading(ctx, ms, thread), true, nil
		}
	}

	thread, found, err := r.threads.FindThread(ctx, ms.MissionDir)
	if err != nil || !found {
		return mission.Reading{}, false, err
	}

	out := r.reading(ctx, ms, thread)
	out.PreviousSessionID = ms.AgentSessionID
	return out, true, nil
}

func (r *Runtime) reading(ctx context.Context, ms mission.Mission, thread ThreadSnapshot) mission.Reading {
	out := reading(thread.ID, thread.Status)
	out.TranscriptPath = thread.Path
	out.HookEpoch = ms.HookEpoch
	if ms.Planning() {
		plans, ok := r.threads.(planReader)
		if ok {
			plan, err := plans.ReadPlan(ctx, thread.ID)
			if err == nil {
				out.PlanKnown, out.PlanPending, out.PlanApproved = plan.Known, plan.Pending, plan.Approved
			}
		}
	}
	return out
}

// Close releases the app-server connection.
func (r *Runtime) Close() error { return r.threads.Close() }

// reading turns an app-server status into q's vocabulary.
func reading(sessionID string, status ThreadStatus) mission.Reading {
	out := mission.Reading{SessionID: sessionID, Activity: status.Classify()}

	switch out.Activity {
	case mission.ActivityWaitingApproval:
		out.WaitingFor = "Codex approval"
	case mission.ActivityWaitingInput:
		out.WaitingFor = "Codex needs input"
	case mission.ActivityFailed:
		out.WaitingFor = "Codex system error"
	}

	return out
}
