package daemon

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/memory"
)

func TestMemoryHTTPClientLifecycle(t *testing.T) {
	srv, _, base := newTestServer(t)
	enableMemory(t, srv.svc)
	ms := launchedServiceMission(t, srv.svc)
	client := api.NewClient(api.Handle{Addr: strings.TrimPrefix(base, "http://"), Token: testToken})
	records, err := client.SaveMemory(t.Context(), api.SaveMemoryRequest{Mission: ms.ID, Records: []memory.Input{{Kind: "lesson", Title: "Cleanup discovery", Body: "Keep unpushed branches", Tags: []string{"git"}}}})
	if err != nil || len(records) != 1 {
		t.Fatalf("save: %v, %v", records, err)
	}
	r := records[0]
	if r.Source.MissionID != string(ms.ID) || r.Source.OperationID != string(ms.OperationID) {
		t.Fatal("missing provenance")
	}
	found, err := client.SearchMemory(t.Context(), memory.Query{Keywords: "cleanup", Mission: string(ms.ID)})
	if err != nil || len(found) != 1 || found[0].Body != "" || found[0].Snippet == "" {
		t.Fatalf("search: %+v, %v", found, err)
	}
	shown, err := client.ShowMemory(t.Context(), r.ID)
	if err != nil || shown.Body != r.Body {
		t.Fatal("show:", err)
	}
	corrected, err := client.UpdateMemory(t.Context(), r.ID, memory.Input{Kind: "lesson", Title: "Corrected discovery", Body: "Evidence changed"})
	if err != nil || corrected.Revision != 2 || corrected.Source.MissionID != r.Source.MissionID {
		t.Fatal("update:", err)
	}
	deleted, err := client.DeleteMemory(t.Context(), r.ID)
	if err != nil || !deleted.Deleted {
		t.Fatal("delete:", err)
	}
	_, err = client.ShowMemory(t.Context(), r.ID)
	status, ok := errors.AsType[*api.StatusError](err)
	if !ok || status.Code != http.StatusNotFound {
		t.Fatalf("deleted show: %v", err)
	}
	_, err = client.SaveMemory(t.Context(), api.SaveMemoryRequest{Mission: ms.ID, Records: []memory.Input{{Kind: "unknown", Title: "x", Body: "y"}}})
	status, ok = errors.AsType[*api.StatusError](err)
	if !ok || status.Code != http.StatusBadRequest {
		t.Fatalf("invalid save: %v", err)
	}
}

func TestMemoryHTTPRequiresAuthenticationAndPairIdentity(t *testing.T) {
	srv, c, base := newTestServer(t)
	enableMemory(t, srv.svc)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/v1/memory/search", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatal("unauthenticated memory access allowed")
	}
	resp = do(t, c, http.MethodPost, base+"/v1/sync/memory", api.MemorySyncRequest{})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatal("unpaired memory exchange allowed")
	}
}
