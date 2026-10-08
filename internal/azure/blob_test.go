package azure

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"

	"github.com/justinrush/q/internal/mission"
)

// blobService is the slice of the blob service a witness talks to: one blob,
// with conditional writes on its ETag.
type blobService struct {
	mu      sync.Mutex
	body    []byte
	version int
	// deny makes every request fail the way a missing role assignment does.
	deny bool
}

func (s *blobService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.deny || r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("x-ms-version") == "" {
		fail(w, http.StatusForbidden, "AuthorizationPermissionMismatch", "This request is not authorized to perform this operation using this permission.")

		return
	}

	if r.URL.Path != "/q/pairs/witness.json" {
		fail(w, http.StatusNotFound, "ContainerNotFound", "The specified container does not exist.")

		return
	}

	etag := `"` + strconv.Itoa(s.version) + `"`

	switch r.Method {
	case http.MethodGet:
		if s.body == nil {
			fail(w, http.StatusNotFound, "BlobNotFound", "The specified blob does not exist.")

			return
		}

		w.Header().Set("ETag", etag)
		_, _ = w.Write(s.body)
	case http.MethodPut:
		if r.Header.Get("If-None-Match") == "*" && s.body != nil {
			fail(w, http.StatusConflict, "BlobAlreadyExists", "The specified blob already exists.")

			return
		}

		if match := r.Header.Get("If-Match"); match != "" && (s.body == nil || match != etag) {
			fail(w, http.StatusPreconditionFailed, "ConditionNotMet", "The condition specified using HTTP conditional header(s) is not met.")

			return
		}

		if r.Header.Get("If-Match") == "" && r.Header.Get("If-None-Match") == "" {
			fail(w, http.StatusBadRequest, "Unconditional", "the witness must never write unconditionally")

			return
		}

		s.body, _ = io.ReadAll(r.Body)
		s.version++

		w.WriteHeader(http.StatusCreated)
	}
}

func fail(w http.ResponseWriter, status int, code, message string) {
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `<?xml version="1.0" encoding="utf-8"?><Error><Code>`+code+`</Code><Message>`+message+"\nRequestId:1</Message></Error>")
}

func newBlob(t *testing.T) (*Blob, *blobService) {
	t.Helper()

	service := &blobService{}
	server := httptest.NewServer(service)
	t.Cleanup(server.Close)

	token := func(context.Context) (string, error) { return "token", nil }

	return NewAt(server.Client(), token, Config{
		Account: "acct", Container: "q", Blob: "pairs/witness.json", Endpoint: server.URL,
	}), service
}

func TestABlobThatDoesNotExistReadsAsNoClaim(t *testing.T) {
	b, _ := newBlob(t)

	claim, err := b.Read(t.Context())
	if err != nil || claim != (mission.Claim{}) {
		t.Fatalf("Read = %+v, %v; want the zero claim", claim, err)
	}
}

func TestAClaimSurvivesTheTripThroughABlob(t *testing.T) {
	b, _ := newBlob(t)
	at := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	if err := b.Write(t.Context(), mission.Claim{Holder: "h_laptop", RenewedAt: at, TTL: 5 * time.Minute}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := b.Read(t.Context())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if got.Holder != "h_laptop" || !got.RenewedAt.Equal(at) || got.TTL != 5*time.Minute || got.Version == "" {
		t.Errorf("claim = %+v, want what was written and a version", got)
	}
}

func TestAWriteAgainstAStaleBlobIsReportedAsARace(t *testing.T) {
	b, _ := newBlob(t)

	if err := b.Write(t.Context(), mission.Claim{Holder: "h_laptop"}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Created by one host while the other also believed there was none.
	if err := b.Write(t.Context(), mission.Claim{Holder: "h_mini"}); !errors.Is(err, mission.ErrClaimRaced) {
		t.Errorf("creating over an existing blob: %v, want ErrClaimRaced", err)
	}

	read, _ := b.Read(t.Context())

	if err := b.Write(t.Context(), mission.Claim{Holder: "h_laptop", Version: read.Version}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if err := b.Write(t.Context(), mission.Claim{Holder: "h_mini", Version: read.Version}); !errors.Is(err, mission.ErrClaimRaced) {
		t.Errorf("updating from a stale ETag: %v, want ErrClaimRaced", err)
	}
}

func TestARefusalCarriesTheServicesCode(t *testing.T) {
	b, service := newBlob(t)
	service.deny = true

	_, err := b.Read(t.Context())
	if err == nil || !strings.Contains(err.Error(), "AuthorizationPermissionMismatch") || strings.Contains(err.Error(), "RequestId") {
		t.Errorf("Read: %v, want the service's code and the first line of its message", err)
	}
}

func TestDefaultsNameTheUsualBlob(t *testing.T) {
	b := NewAt(http.DefaultClient, nil, Config{Account: "acct"})

	if b.Name() != "azure:acct/q/witness.json" || b.url != "https://acct.blob.core.windows.net/q/witness.json" {
		t.Errorf("name %q url %q, want the defaults", b.Name(), b.url)
	}
}

// countingCredential hands out tokens with a fixed lifetime and counts them.
type countingCredential struct {
	now   func() time.Time
	calls int
}

func (c *countingCredential) GetToken(_ context.Context, opts policy.TokenRequestOptions) (azcore.AccessToken, error) {
	c.calls++

	return azcore.AccessToken{Token: opts.Scopes[0] + "#" + strconv.Itoa(c.calls), ExpiresOn: c.now().Add(time.Hour)}, nil
}

// The az-login credential shells out for every token, so one has to last.
func TestATokenIsReusedUntilShortlyBeforeItExpires(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	credential := &countingCredential{now: clock}
	token := cachedToken(credential, clock)

	first, _ := token(t.Context())
	now = now.Add(50 * time.Minute)
	second, _ := token(t.Context())

	if first != second || credential.calls != 1 || !strings.HasPrefix(first, storageScope) {
		t.Fatalf("tokens %q then %q after %d calls, want one storage token reused", first, second, credential.calls)
	}

	now = now.Add(9 * time.Minute)

	if third, _ := token(t.Context()); third == first || credential.calls != 2 {
		t.Errorf("token %q after %d calls, want a fresh one inside the last two minutes", third, credential.calls)
	}
}
