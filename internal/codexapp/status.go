package codexapp

import (
	"encoding/json"
	"fmt"
)

// Activity is Q's stable interpretation of a Codex thread runtime status.
type Activity string

const (
	ActivityUnknown         Activity = "unknown"
	ActivityBusy            Activity = "busy"
	ActivityWaitingApproval Activity = "waiting_approval"
	ActivityWaitingInput    Activity = "waiting_input"
	ActivityIdle            Activity = "idle"
	ActivityFailed          Activity = "failed"
)

const (
	statusNotLoaded   = "notLoaded"
	statusActive      = "active"
	statusIdle        = "idle"
	statusSystemError = "systemError"

	flagWaitingApproval = "waitingOnApproval"
	flagWaitingInput    = "waitingOnUserInput"
)

// ThreadStatus is the runtime status included in thread/status/changed.
type ThreadStatus struct {
	Type        string   `json:"type"`
	ActiveFlags []string `json:"activeFlags,omitempty"`
}

// ThreadStatusChanged is the payload of thread/status/changed.
type ThreadStatusChanged struct {
	ThreadID string       `json:"threadId"`
	Status   ThreadStatus `json:"status"`
}

// Classify maps app-server's wire status to the states Q acts on.
func (s ThreadStatus) Classify() Activity {
	if s.Type == statusActive {
		for _, flag := range s.ActiveFlags {
			if flag == flagWaitingApproval {
				return ActivityWaitingApproval
			}
		}

		for _, flag := range s.ActiveFlags {
			if flag == flagWaitingInput {
				return ActivityWaitingInput
			}
		}

		return ActivityBusy
	}

	switch s.Type {
	case statusIdle:
		return ActivityIdle
	case statusSystemError:
		return ActivityFailed
	case statusNotLoaded:
		return ActivityUnknown
	default:
		return ActivityUnknown
	}
}

// DecodeThreadStatus decodes a status notification and reports whether the
// notification was the expected method.
func DecodeThreadStatus(notification Notification) (ThreadStatusChanged, bool, error) {
	if notification.Method != "thread/status/changed" {
		return ThreadStatusChanged{}, false, nil
	}

	var changed ThreadStatusChanged
	err := json.Unmarshal(notification.Params, &changed)
	if err != nil {
		return ThreadStatusChanged{}, true, fmt.Errorf("decoding thread status: %w", err)
	}

	if changed.ThreadID == "" || changed.Status.Type == "" {
		return ThreadStatusChanged{}, true, fmt.Errorf("decoding thread status: missing thread id or status type")
	}

	return changed, true, nil
}
