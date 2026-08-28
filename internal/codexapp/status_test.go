package codexapp

import (
	"encoding/json"
	"testing"
)

func TestThreadStatusClassify(t *testing.T) {
	tests := map[string]struct {
		status ThreadStatus
		want   Activity
	}{
		"active": {
			status: ThreadStatus{Type: "active"},
			want:   ActivityBusy,
		},
		"approval": {
			status: ThreadStatus{Type: "active", ActiveFlags: []string{"waitingOnApproval"}},
			want:   ActivityWaitingApproval,
		},
		"input": {
			status: ThreadStatus{Type: "active", ActiveFlags: []string{"waitingOnUserInput"}},
			want:   ActivityWaitingInput,
		},
		"approval wins over input": {
			status: ThreadStatus{Type: "active", ActiveFlags: []string{"waitingOnUserInput", "waitingOnApproval"}},
			want:   ActivityWaitingApproval,
		},
		"idle": {
			status: ThreadStatus{Type: "idle"},
			want:   ActivityIdle,
		},
		"system error": {
			status: ThreadStatus{Type: "systemError"},
			want:   ActivityFailed,
		},
		"not loaded": {
			status: ThreadStatus{Type: "notLoaded"},
			want:   ActivityUnknown,
		},
		"future status": {
			status: ThreadStatus{Type: "newStatus"},
			want:   ActivityUnknown,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if got := test.status.Classify(); got != test.want {
				t.Errorf("Classify() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestDecodeThreadStatus(t *testing.T) {
	tests := map[string]struct {
		notification Notification
		wantMatched  bool
		wantErr      bool
	}{
		"status": {
			notification: Notification{
				Method: "thread/status/changed",
				Params: json.RawMessage(`{"threadId":"thr-1","status":{"type":"active","activeFlags":[]}}`),
			},
			wantMatched: true,
		},
		"other event": {
			notification: Notification{Method: "turn/started", Params: json.RawMessage(`{}`)},
		},
		"bad payload": {
			notification: Notification{Method: "thread/status/changed", Params: json.RawMessage(`{`)},
			wantMatched:  true,
			wantErr:      true,
		},
		"missing fields": {
			notification: Notification{Method: "thread/status/changed", Params: json.RawMessage(`{}`)},
			wantMatched:  true,
			wantErr:      true,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, matched, err := DecodeThreadStatus(test.notification)
			if matched != test.wantMatched {
				t.Errorf("matched = %v, want %v", matched, test.wantMatched)
			}

			if (err != nil) != test.wantErr {
				t.Errorf("error = %v, wantErr %v", err, test.wantErr)
			}

			if matched && err == nil && got.ThreadID != "thr-1" {
				t.Errorf("ThreadID = %q", got.ThreadID)
			}
		})
	}
}
