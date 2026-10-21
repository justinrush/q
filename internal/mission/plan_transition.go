package mission

import (
	"context"
	"time"
)

// TurnSettings is observed execution evidence, never inferred from launch argv.
type TurnSettings struct {
	SessionID string    `json:"sessionId,omitempty"`
	TurnID    string    `json:"turnId,omitempty"`
	Mode      string    `json:"mode,omitempty"`
	Model     string    `json:"model,omitempty"`
	Effort    string    `json:"effort,omitempty"`
	At        time.Time `json:"at,omitzero"`
}

// PlanTransition tracks an unverified launch or an approved model handoff.
// Verified transitions retain their boundary so old rollout records cannot
// validate a later launch. Failed transitions require an explicit retry.
type PlanTransition struct {
	State     string    `json:"state,omitempty"`
	Phase     Phase     `json:"phase,omitempty"`
	SessionID string    `json:"sessionId,omitempty"`
	HookEpoch int       `json:"hookEpoch,omitempty"`
	Since     time.Time `json:"since,omitzero"`
	TurnID    string    `json:"turnId,omitempty"`
	Error     string    `json:"error,omitempty"`
	Message   string    `json:"message,omitempty"`
}

const (
	PlanTransitionPending   = "pending"
	PlanTransitionVerifying = "verifying"
	PlanTransitionVerified  = "verified"
	PlanTransitionFailed    = "failed"
)

// PlanController changes a live runtime without weakening its permissions.
// ConfigureImplementation changes defaults without starting generation.
type PlanController interface {
	Interrupt(context.Context, string) error
	ConfigureImplementation(context.Context, Mission) error
	StartImplementation(context.Context, Mission, string) (string, error)
}

func (m Mission) PlanFailed() bool { return m.PlanTransition.State == PlanTransitionFailed }

// BeginPlanVerification records the launch boundary before the agent starts.
func (m *Mission) BeginPlanVerification(now time.Time) {
	if m.Tool != ToolCodex || !m.PlanMode {
		return
	}
	phase := PhasePlan
	if m.PlanApproved {
		phase = PhaseImplement
	}
	m.PlanTransition = PlanTransition{State: PlanTransitionVerifying, Phase: phase,
		SessionID: m.AgentSessionID, HookEpoch: m.HookEpoch, Since: now, Message: m.PlanTransition.Message}
}
