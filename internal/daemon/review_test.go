package daemon

import (
	"errors"
	"testing"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
)

// seedReviewable stores a mission under operation and applies mutate to it, so
// a case can describe the one way its mission differs from a launched one.
func seedReviewable(t *testing.T, svc *Service, operation mission.Operation, mutate func(*mission.Mission)) mission.Mission {
	t.Helper()

	ms := seedParentMission(t, svc, operation)
	started := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

	if err := svcStore(svc).Apply("test.reviewable", func(snap *mission.Snapshot) error {
		stored, _ := snap.Mission(ms.ID)
		stored.StartedAt = &started
		stored.Status = mission.StatusDebrief

		if mutate != nil {
			mutate(&stored)
		}

		ms = stored
		snap.PutMission(stored)

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	return ms
}

func TestCreateMissionReview(t *testing.T) {
	cases := []struct {
		name    string
		source  func(*mission.Mission)
		req     api.CreateMissionRequest
		wantErr error
		check   func(t *testing.T, svc *Service, source, review mission.Mission)
	}{
		{
			name: "named by q and briefed with the default focus",
			req:  api.CreateMissionRequest{Tool: mission.ToolCodex, Model: "gpt-5.4", Effort: "high", Queued: true},
			check: func(t *testing.T, _ *Service, source, review mission.Mission) {
				if review.Name != "Review Wire Up The API" || review.Slug != "review-wire-up-the-api" {
					t.Errorf("Name/Slug = %q/%q", review.Name, review.Slug)
				}

				if review.Prompt != mission.DefaultReviewFocus {
					t.Errorf("Prompt = %q, want the default focus", review.Prompt)
				}

				if review.Tool != mission.ToolCodex || review.Model != "gpt-5.4" || review.Effort != "high" {
					t.Errorf("agent = %s/%s/%s, want the reviewer asked for", review.Tool, review.Model, review.Effort)
				}

				if review.OperationID != source.OperationID {
					t.Errorf("OperationID = %q, want the reviewed mission's", review.OperationID)
				}

				if review.Status != mission.StatusBriefing || !review.Queued || review.Launched() {
					t.Errorf("review should be a queued brief: %+v", review)
				}
			},
		},
		{
			name: "keeps the name and focus it was given",
			req: api.CreateMissionRequest{
				Tool: mission.ToolCodex, Name: "Second opinion", Prompt: "Check the migrations.",
			},
			check: func(t *testing.T, _ *Service, _, review mission.Mission) {
				if review.Name != "Second opinion" || review.Prompt != "Check the migrations." {
					t.Errorf("Name/Prompt = %q/%q", review.Name, review.Prompt)
				}
			},
		},
		{
			name: "takes no repositories of its own",
			req: api.CreateMissionRequest{
				Tool:         mission.ToolCodex,
				ExtraRepos:   []mission.Repo{{Name: "q", Path: "/dev/q"}},
				BaseBranches: map[string]string{"q": "main"},
			},
			check: func(t *testing.T, _ *Service, _, review mission.Mission) {
				if len(review.ExtraRepos) != 0 || len(review.BaseBranches) != 0 {
					t.Errorf("review kept repos %+v and bases %+v", review.ExtraRepos, review.BaseBranches)
				}
			},
		},
		{
			name: "is pinned to the host running the reviewed mission",
			req:  api.CreateMissionRequest{Tool: mission.ToolCodex},
			check: func(t *testing.T, svc *Service, source, review mission.Mission) {
				if source.Lease.Holder != svc.self || review.Pin != svc.self {
					t.Errorf("Pin = %q with holder %q, want this host %q", review.Pin, source.Lease.Holder, svc.self)
				}
			},
		},
		{
			name:   "is left unpinned when the holder is a host this one no longer knows",
			source: func(ms *mission.Mission) { ms.Lease = mission.Lease{Holder: "h_aabbccddeeff", Epoch: 2} },
			req:    api.CreateMissionRequest{Tool: mission.ToolCodex},
			check: func(t *testing.T, _ *Service, _, review mission.Mission) {
				if review.Pin != "" {
					t.Errorf("Pin = %q, want none", review.Pin)
				}
			},
		},
		{
			name:    "needs a tool",
			req:     api.CreateMissionRequest{},
			wantErr: ErrInvalid,
		},
		{
			name:    "cannot start in plan mode",
			req:     api.CreateMissionRequest{Tool: mission.ToolClaude, PlanMode: true},
			wantErr: ErrInvalid,
		},
		{
			name:    "rejects an unusable model",
			req:     api.CreateMissionRequest{Tool: mission.ToolCodex, Model: "--yolo"},
			wantErr: ErrInvalid,
		},
		{
			name:    "refuses a mission that never ran",
			source:  func(ms *mission.Mission) { ms.StartedAt = nil; ms.Status = mission.StatusBriefing },
			req:     api.CreateMissionRequest{Tool: mission.ToolCodex},
			wantErr: ErrInvalid,
		},
		{
			name:    "refuses a closed mission",
			source:  func(ms *mission.Mission) { ms.Status = mission.StatusClosed },
			req:     api.CreateMissionRequest{Tool: mission.ToolCodex},
			wantErr: ErrInvalid,
		},
		{
			name:    "refuses to review a review",
			source:  func(ms *mission.Mission) { ms.ReviewOf = "ms_ffeeddccbbaa" },
			req:     api.CreateMissionRequest{Tool: mission.ToolCodex},
			wantErr: ErrInvalid,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestService(t)
			source := seedReviewable(t, svc, seedOperation(t, svc), tc.source)

			tc.req.ReviewOf = source.ID

			review, err := svc.CreateMission(tc.req)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}

			if tc.wantErr != nil {
				return
			}

			if review.ReviewOf != source.ID {
				t.Errorf("ReviewOf = %q, want %q", review.ReviewOf, source.ID)
			}

			tc.check(t, svc, source, review)
		})
	}
}

func TestCreateMissionReviewOfUnknownMission(t *testing.T) {
	svc := newTestService(t)
	seedOperation(t, svc)

	_, err := svc.CreateMission(api.CreateMissionRequest{ReviewOf: "ms_does_not_exist", Tool: mission.ToolCodex})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}
