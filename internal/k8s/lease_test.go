package k8s

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justinrush/q/internal/mission"
)

// apiServer is the slice of a Kubernetes API server a Lease talks to: one
// object, optimistic concurrency on its resourceVersion.
type apiServer struct {
	mu      sync.Mutex
	stored  *lease
	version int
	// deny makes every request fail the way a missing role binding does.
	deny bool
}

func (a *apiServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()

	const collection = "/apis/coordination.k8s.io/v1/namespaces/q/leases"

	if a.deny {
		writeStatus(w, http.StatusForbidden, `leases.coordination.k8s.io "pair" is forbidden: User "laptop" cannot get resource "leases"`)

		return
	}

	switch {
	case r.Method == http.MethodGet && r.URL.Path == collection+"/pair":
		if a.stored == nil {
			writeStatus(w, http.StatusNotFound, "not found")

			return
		}

		_ = json.NewEncoder(w).Encode(a.stored)
	case r.Method == http.MethodPost && r.URL.Path == collection:
		if a.stored != nil {
			writeStatus(w, http.StatusConflict, "already exists")

			return
		}

		a.store(w, r, http.StatusCreated)
	case r.Method == http.MethodPut && r.URL.Path == collection+"/pair":
		var sent lease

		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &sent)

		if a.stored == nil || sent.Metadata.ResourceVersion != a.stored.Metadata.ResourceVersion {
			writeStatus(w, http.StatusConflict, "the object has been modified")

			return
		}

		r.Body = io.NopCloser(strings.NewReader(string(body)))
		a.store(w, r, http.StatusOK)
	default:
		writeStatus(w, http.StatusMethodNotAllowed, r.Method+" "+r.URL.Path)
	}
}

func (a *apiServer) store(w http.ResponseWriter, r *http.Request, code int) {
	var sent lease
	if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
		writeStatus(w, http.StatusBadRequest, err.Error())

		return
	}

	a.version++
	sent.Metadata.ResourceVersion = strconv.Itoa(a.version)
	a.stored = &sent

	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(sent)
}

func writeStatus(w http.ResponseWriter, code int, message string) {
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "message": message, "code": code})
}

func newLease(t *testing.T) (*Lease, *apiServer) {
	t.Helper()

	api := &apiServer{}
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)

	return NewAt(server.Client(), server.URL+"/", "q", "pair"), api
}

func TestALeaseThatDoesNotExistReadsAsNoClaim(t *testing.T) {
	l, _ := newLease(t)

	claim, err := l.Read(t.Context())
	if err != nil || claim != (mission.Claim{}) {
		t.Fatalf("Read = %+v, %v; want the zero claim", claim, err)
	}
}

func TestAClaimSurvivesTheTripThroughALease(t *testing.T) {
	l, api := newLease(t)
	at := time.Date(2026, 10, 8, 12, 0, 0, 123456000, time.UTC)

	if err := l.Write(t.Context(), mission.Claim{Holder: "h_laptop", RenewedAt: at, TTL: 5 * time.Minute}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := l.Read(t.Context())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if got.Holder != "h_laptop" || !got.RenewedAt.Equal(at) || got.TTL != 5*time.Minute || got.Version == "" {
		t.Errorf("claim = %+v, want what was written and a version", got)
	}

	// It is a Lease any other tool would recognize.
	if api.stored.Kind != "Lease" || api.stored.Spec.RenewTime != "2026-10-08T12:00:00.123456Z" {
		t.Errorf("stored %+v, want a well-formed Lease", api.stored)
	}

	// A claim with no expiry leaves the duration out, which the API requires
	// to be positive when present.
	got.TTL, got.Holder = 0, "h_mini"
	if err := l.Write(t.Context(), got); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if api.stored.Spec.LeaseDurationSeconds != nil {
		t.Errorf("leaseDurationSeconds = %d, want it omitted", *api.stored.Spec.LeaseDurationSeconds)
	}

	if again, _ := l.Read(t.Context()); again.TTL != 0 || again.Holder != "h_mini" {
		t.Errorf("claim = %+v, want an unexpiring claim by the new holder", again)
	}
}

func TestAWriteAgainstAStaleLeaseIsReportedAsARace(t *testing.T) {
	l, _ := newLease(t)

	if err := l.Write(t.Context(), mission.Claim{Holder: "h_laptop"}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Created by one host while the other also believed there was none.
	if err := l.Write(t.Context(), mission.Claim{Holder: "h_mini"}); !errors.Is(err, mission.ErrClaimRaced) {
		t.Errorf("creating over an existing lease: %v, want ErrClaimRaced", err)
	}

	read, _ := l.Read(t.Context())

	if err := l.Write(t.Context(), mission.Claim{Holder: "h_laptop", Version: read.Version}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if err := l.Write(t.Context(), mission.Claim{Holder: "h_mini", Version: read.Version}); !errors.Is(err, mission.ErrClaimRaced) {
		t.Errorf("updating from a stale version: %v, want ErrClaimRaced", err)
	}
}

func TestARefusalCarriesTheClustersExplanation(t *testing.T) {
	l, api := newLease(t)
	api.deny = true

	_, err := l.Read(t.Context())
	if err == nil || !strings.Contains(err.Error(), `cannot get resource "leases"`) || errors.Is(err, mission.ErrClaimRaced) {
		t.Errorf("Read: %v, want the API server's message", err)
	}
}

func TestTheNameLeavesOutTheCluster(t *testing.T) {
	a := NewAt(http.DefaultClient, "https://10.0.0.1:6443", "", "")
	b := NewAt(http.DefaultClient, "https://k8s.home.example", "default", "q-witness")

	if a.Name() != b.Name() || a.Name() != "kubernetes:default/q-witness" {
		t.Errorf("names %q and %q, want one name for one lease reached two ways", a.Name(), b.Name())
	}
}
