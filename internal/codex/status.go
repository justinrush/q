package codex

import (
	"encoding/json"
	"fmt"

	"github.com/justinrush/q/internal/mission"
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

// Classify maps app-server's wire status to the activities q acts on.
func (s ThreadStatus) Classify() mission.Activity {
	if s.Type == statusActive {
		for _, flag := range s.ActiveFlags {
			if flag == flagWaitingApproval {
				return mission.ActivityWaitingApproval
			}
		}

		for _, flag := range s.ActiveFlags {
			if flag == flagWaitingInput {
				return mission.ActivityWaitingInput
			}
		}

		return mission.ActivityBusy
	}

	switch s.Type {
	case statusIdle:
		return mission.ActivityIdle
	case statusSystemError:
		return mission.ActivityFailed
	case statusNotLoaded:
		return mission.ActivityUnknown
	default:
		return mission.ActivityUnknown
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
