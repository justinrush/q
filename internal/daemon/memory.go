package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/memory"
	"github.com/justinrush/q/internal/mission"
)

func WithMemory(store *memory.Store, capture bool) Option {
	return func(s *Service) { s.memory = store; s.captureOnFinish = capture }
}

func memoryError(err error) error {
	if errors.Is(err, memory.ErrInvalid) {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if errors.Is(err, memory.ErrNotFound) {
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	return err
}

func (s *Service) SaveMemory(ctx context.Context, req api.SaveMemoryRequest) ([]memory.Record, error) {
	if s.memory == nil {
		return nil, fmt.Errorf("%w: memory is unavailable", ErrConflict)
	}
	s.finishMu.Lock()
	defer s.finishMu.Unlock()
	ms, op, err := s.missionWithOperation(req.Mission)
	if err != nil {
		return nil, err
	}
	if req.Capture != "" {
		if !s.validCapture(ms, req.Capture) {
			return nil, fmt.Errorf("%w: stale or canceled capture", ErrConflict)
		}
		summaries := 0
		for _, in := range req.Records {
			if in.Kind == "summary" {
				summaries++
			}
		}
		if summaries != 1 {
			return nil, fmt.Errorf("%w: capture requires exactly one summary", ErrInvalid)
		}
	}
	source := memory.Source{MissionID: string(ms.ID), MissionName: ms.Name, OperationID: string(ms.OperationID), OperationName: op.Name, Agent: string(ms.Tool)}
	repos, err := mission.MissionRepos(op, ms)
	if err != nil {
		return nil, err
	}
	if ms.IsReview() {
		if parent, ok := s.store.Snapshot().Mission(ms.ReviewOf); ok {
			repos, err = mission.MissionRepos(op, parent)
			if err != nil {
				return nil, err
			}
		}
	}
	for _, repo := range repos {
		identity := repo.URL
		if identity == "" && s.locator != nil && repo.Path != "" {
			identity, _ = s.locator.OriginURL(ctx, repo.Path)
		}
		if identity == "" {
			identity = string(ms.OperationID) + "/" + repo.Name
		}
		source.Repos = append(source.Repos, memory.Repo{Name: repo.Name, Identity: identity})
	}
	records, err := s.memory.Save(req.Records, source, req.Capture, s.now())
	if err != nil {
		return nil, memoryError(err)
	}
	if req.Capture != "" && ms.Finish.AcknowledgedAt.IsZero() {
		err = s.changeFinish(ms.ID, func(m *mission.Mission) {
			m.Finish.AcknowledgedAt = s.now()
			m.Finish.Phase = mission.FinishAwaitingIdle
		})
	}
	s.kickSync()
	return records, err
}

func (s *Server) memoryRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/memory", func(w http.ResponseWriter, r *http.Request) {
		var req api.SaveMemoryRequest
		if !decode(w, r, &req) {
			return
		}
		out, err := s.svc.SaveMemory(r.Context(), req)
		memoryReply(w, out, err)
	})
	mux.HandleFunc("POST /v1/memory/search", func(w http.ResponseWriter, r *http.Request) {
		var q memory.Query
		if !decode(w, r, &q) {
			return
		}
		if s.svc.memory == nil {
			memoryReply(w, nil, ErrConflict)
			return
		}
		results := s.svc.memory.Search(q)
		if err := s.svc.recordMemoryQuery(q, len(results)); err != nil {
			s.svc.warn("recording memory query", "error", err)
		}
		writeJSON(w, http.StatusOK, results)
	})
	mux.HandleFunc("GET /v1/memory/{id}", func(w http.ResponseWriter, r *http.Request) {
		if s.svc.memory == nil {
			memoryReply(w, nil, ErrConflict)
			return
		}
		out, err := s.svc.memory.Get(r.PathValue("id"))
		memoryReply(w, out, err)
	})
	mux.HandleFunc("PATCH /v1/memory/{id}", func(w http.ResponseWriter, r *http.Request) {
		var in memory.Input
		if !decode(w, r, &in) {
			return
		}
		if s.svc.memory == nil {
			memoryReply(w, nil, ErrConflict)
			return
		}
		out, err := s.svc.memory.Update(r.PathValue("id"), in, false, s.svc.now())
		memoryReply(w, out, err)
		if err == nil {
			s.svc.kickSync()
		}
	})
	mux.HandleFunc("DELETE /v1/memory/{id}", func(w http.ResponseWriter, r *http.Request) {
		if s.svc.memory == nil {
			memoryReply(w, nil, ErrConflict)
			return
		}
		out, err := s.svc.memory.Update(r.PathValue("id"), memory.Input{}, true, s.svc.now())
		memoryReply(w, out, err)
		if err == nil {
			s.svc.kickSync()
		}
	})
	mux.HandleFunc("POST /v1/sync/memory", func(w http.ResponseWriter, r *http.Request) {
		var req api.MemorySyncRequest
		if !decode(w, r, &req) {
			return
		}
		out, err := s.svc.MemoryExchange(req)
		memoryReply(w, out, err)
	})
}

func memoryReply(w http.ResponseWriter, out any, err error) {
	if err != nil {
		writeServiceError(w, memoryError(err))
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type memoryLink struct {
	mu     sync.Mutex
	status string
}

func (l *memoryLink) set(v string) { l.mu.Lock(); defer l.mu.Unlock(); l.status = v }
func (l *memoryLink) get() string  { l.mu.Lock(); defer l.mu.Unlock(); return l.status }

type memoryRemote interface {
	MemorySync(context.Context, api.MemorySyncRequest) (api.MemorySyncResponse, error)
}

func (s *Service) MemoryExchange(req api.MemorySyncRequest) (api.MemorySyncResponse, error) {
	snap := s.store.Snapshot()
	if s.memory == nil || s.remote != nil || snap.PeerID() != req.From || req.Expect != s.self || req.From == "" {
		return api.MemorySyncResponse{}, fmt.Errorf("%w: memory exchange requires the paired primary", ErrConflict)
	}
	if len(req.Want) > memory.TransferSize {
		return api.MemorySyncResponse{}, fmt.Errorf("%w: too many requested memories", ErrInvalid)
	}
	if len(req.Records) > 0 {
		if err := s.memory.Merge(req.Records, false); err != nil {
			return api.MemorySyncResponse{}, memoryError(err)
		}
	}
	entries, next := s.memory.Manifest(req.After)
	records, err := s.memory.Records(req.Want)
	if err == nil {
		s.memoryLink.set("available")
	}
	return api.MemorySyncResponse{Manifest: entries, Next: next, Records: records}, err
}

func (s *Service) syncMemory(ctx context.Context, supported bool) {
	peer, ok := s.remote.(memoryRemote)
	if !supported || !ok || s.memory == nil {
		s.memoryLink.set("unavailable: upgrade both paired daemons")
		return
	}
	err := s.exchangeMemory(ctx, peer)
	if err != nil {
		s.memoryLink.set(err.Error())
		s.warn("synchronizing memory", "error", err)
	} else {
		s.memoryLink.set("available")
	}
	s.publishRemote()
}

func (s *Service) exchangeMemory(ctx context.Context, peer memoryRemote) error {
	snap := s.store.Snapshot()
	base := api.MemorySyncRequest{From: s.self, Expect: snap.PeerID()}
	theirs, err := remoteMemoryManifest(ctx, peer, base)
	if err != nil {
		return err
	}

	mine := map[string]memory.Entry{}
	after := ""
	for {
		entries, next := s.memory.Manifest(after)
		for _, e := range entries {
			mine[e.ID] = e
		}
		if next == "" {
			break
		}
		after = next
	}
	pull, push := []string{}, []string{}
	for id, remote := range theirs {
		local, exists := mine[id]
		if !exists || memory.IncomingWins(local, remote, true) {
			pull = append(pull, id)
		}
	}
	for id, local := range mine {
		remote, exists := theirs[id]
		if !exists || memory.IncomingWins(remote, local, false) {
			push = append(push, id)
		}
	}
	base.After = ""
	for len(pull) > 0 {
		n := min(len(pull), memory.TransferSize)
		base.Want = pull[:n]
		resp, err := peer.MemorySync(ctx, base)
		if err != nil {
			return err
		}
		if err = s.memory.Merge(resp.Records, true); err != nil {
			return err
		}
		pull = pull[n:]
	}
	base.Want = nil
	for len(push) > 0 {
		n := min(len(push), memory.TransferSize)
		records, err := s.memory.Records(push[:n])
		if err != nil {
			return err
		}
		base.Records = records
		if _, err = peer.MemorySync(ctx, base); err != nil {
			return err
		}
		push = push[n:]
	}
	return nil
}

func remoteMemoryManifest(ctx context.Context, peer memoryRemote, req api.MemorySyncRequest) (map[string]memory.Entry, error) {
	out := map[string]memory.Entry{}
	for {
		resp, err := peer.MemorySync(ctx, req)
		if err != nil {
			return nil, err
		}
		for _, e := range resp.Manifest {
			out[e.ID] = e
		}
		if resp.Next == "" {
			return out, nil
		}
		if resp.Next <= req.After {
			return nil, errors.New("memory manifest cursor did not advance")
		}
		req.After = resp.Next
	}
}

var memoryQueryMu sync.Mutex

type memoryQueryEvent struct {
	At            time.Time `json:"at"`
	Tool          string    `json:"tool"`
	Mission       string    `json:"mission,omitempty"`
	Keywords      string    `json:"keywords"`
	Repo          string    `json:"repo,omitempty"`
	SourceMission string    `json:"sourceMission,omitempty"`
	Operation     string    `json:"operation,omitempty"`
	Kind          string    `json:"kind,omitempty"`
	Limit         int       `json:"limit,omitempty"`
	Results       int       `json:"results"`
}

func (s *Service) recordMemoryQuery(q memory.Query, results int) error {
	event := memoryQueryEvent{At: s.now().UTC(), Tool: "unknown", Keywords: q.Keywords, Repo: q.Repo, SourceMission: q.Mission, Operation: q.Operation, Kind: q.Kind, Limit: q.Limit, Results: results}
	if q.CallerMission != "" {
		if ms, ok := s.store.Snapshot().Mission(mission.MissionID(q.CallerMission)); ok {
			event.Mission = string(ms.ID)
			event.Tool = string(ms.Tool)
		}
	}
	memoryQueryMu.Lock()
	defer memoryQueryMu.Unlock()
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	path := filepath.Join(s.dirs.Data, "memory-queries.jsonl")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(data, '\n'))
	return err
}
