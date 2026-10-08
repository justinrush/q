// Package azure keeps a pair's witness claim in a blob in an Azure storage
// account.
//
// The claim is a small JSON document. What makes a blob usable as a witness is
// its ETag: a write can be made conditional on the blob being unchanged since
// it was read, or on it not existing, and the service refuses it otherwise.
// Blob leases are not used. They last sixty seconds at most or for ever, and
// neither is a window measured in minutes that has to survive a closed lid.
//
// Requests are signed with a token from the ambient Azure identity: az login
// on a laptop, a managed identity on a VM. That identity needs the Storage
// Blob Data Contributor role on the container.
package azure

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	"github.com/justinrush/q/internal/mission"
)

// Defaults for a blob nobody named.
const (
	DefaultContainer = "q"
	DefaultBlob      = "witness.json"
)

const (
	// storageScope is the audience of a token for the blob service.
	storageScope = "https://storage.azure.com/.default"
	// apiVersion is the blob service version the requests are written against.
	apiVersion = "2023-11-03"
	// tokenSlack is how long before a token expires it is replaced.
	tokenSlack = 2 * time.Minute
	// bodyLimit bounds how much of a response is read.
	bodyLimit = 1 << 20
)

// Config says which blob to use.
type Config struct {
	// Account is the storage account's name.
	Account string
	// Container and Blob locate the document within it.
	Container string
	Blob      string
	// Endpoint replaces https://<account>.blob.core.windows.net, for a
	// sovereign cloud, a private endpoint with its own name, or an emulator.
	Endpoint string
}

// TokenSource returns a bearer token for the blob service.
type TokenSource func(ctx context.Context) (string, error)

// Blob is a witness backed by one blob.
type Blob struct {
	client *http.Client
	token  TokenSource
	url    string
	name   string
}

// New returns the blob the configuration names, signed for with the default
// Azure credential chain. Nothing is contacted until the first read.
func New(cfg Config) (*Blob, error) {
	if cfg.Account == "" {
		return nil, fmt.Errorf("an Azure witness needs a storage account name")
	}

	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("finding Azure credentials: %w", err)
	}

	return NewAt(http.DefaultClient, cachedToken(credential, time.Now), cfg), nil
}

// NewAt returns a blob reached with the given client and tokens. An empty
// container or blob name takes the default.
func NewAt(client *http.Client, token TokenSource, cfg Config) *Blob {
	if cfg.Container == "" {
		cfg.Container = DefaultContainer
	}

	if cfg.Blob == "" {
		cfg.Blob = DefaultBlob
	}

	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = "https://" + cfg.Account + ".blob.core.windows.net"
	}

	return &Blob{
		client: client,
		token:  token,
		url:    strings.TrimRight(endpoint, "/") + "/" + url.PathEscape(cfg.Container) + "/" + escapePath(cfg.Blob),
		name:   "azure:" + cfg.Account + "/" + cfg.Container + "/" + cfg.Blob,
	}
}

// escapePath escapes a blob name segment by segment, keeping the slashes that
// give it the appearance of a path.
func escapePath(name string) string {
	parts := strings.Split(name, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}

	return strings.Join(parts, "/")
}

// cachedToken wraps a credential so a token is fetched once and reused until
// shortly before it expires. The chain's az-login credential runs the az CLI
// for every token it is asked for, and the witness is asked every few seconds.
func cachedToken(credential azcore.TokenCredential, now func() time.Time) TokenSource {
	var (
		mu      sync.Mutex
		current azcore.AccessToken
	)

	return func(ctx context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()

		if current.Token != "" && now().Before(current.ExpiresOn.Add(-tokenSlack)) {
			return current.Token, nil
		}

		fresh, err := credential.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{storageScope}})
		if err != nil {
			return "", fmt.Errorf("getting an Azure token for the storage account: %w", err)
		}

		current = fresh

		return current.Token, nil
	}
}

// Name identifies the blob by account, container and name.
func (b *Blob) Name() string { return b.name }

// Read returns the claim the blob holds, the zero Claim when it does not exist.
func (b *Blob) Read(ctx context.Context) (mission.Claim, error) {
	status, etag, body, err := b.do(ctx, http.MethodGet, nil, nil)
	if err != nil {
		return mission.Claim{}, err
	}

	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		// A missing container answers the same way. The write that follows
		// fails on it and says so.
		return mission.Claim{}, nil
	default:
		return mission.Claim{}, b.refusal("reading", status, body)
	}

	var claim mission.Claim
	if err := json.Unmarshal(body, &claim); err != nil {
		return mission.Claim{}, fmt.Errorf("decoding %s: %w", b.name, err)
	}

	claim.Version = etag
	if claim.Version == "" {
		return mission.Claim{}, fmt.Errorf("%s was returned without an ETag, so it cannot be written safely", b.name)
	}

	return claim, nil
}

// Write replaces the blob if it is unchanged since the claim was read, or
// creates it if the claim was read as absent. A refusal on either condition is
// reported as [mission.ErrClaimRaced].
func (b *Blob) Write(ctx context.Context, claim mission.Claim) error {
	payload, err := json.Marshal(claim)
	if err != nil {
		return fmt.Errorf("encoding %s: %w", b.name, err)
	}

	headers := map[string]string{
		"x-ms-blob-type": "BlockBlob",
		"Content-Type":   "application/json",
	}

	if claim.Version == "" {
		headers["If-None-Match"] = "*"
	} else {
		headers["If-Match"] = claim.Version
	}

	status, _, body, err := b.do(ctx, http.MethodPut, payload, headers)
	if err != nil {
		return err
	}

	switch status {
	case http.StatusCreated, http.StatusOK:
		return nil
	case http.StatusPreconditionFailed, http.StatusConflict:
		// 412 is a failed If-Match; 409 is a create that found the blob there.
		return mission.ErrClaimRaced
	default:
		return b.refusal("writing", status, body)
	}
}

// do makes one signed request and returns the status, the blob's ETag, and
// the body.
func (b *Blob) do(
	ctx context.Context,
	method string,
	payload []byte,
	headers map[string]string,
) (status int, etag string, body []byte, err error) {
	token, err := b.token(ctx)
	if err != nil {
		return 0, "", nil, err
	}

	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, b.url, reader)
	if err != nil {
		return 0, "", nil, err
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("x-ms-version", apiVersion)
	req.Header.Set("x-ms-date", time.Now().UTC().Format(http.TimeFormat))

	for key, value := range headers {
		req.Header.Set(key, value)
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return 0, "", nil, fmt.Errorf("reaching the storage account for %s: %w", b.name, err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, err = io.ReadAll(io.LimitReader(resp.Body, bodyLimit))
	if err != nil {
		return 0, "", nil, fmt.Errorf("reading the storage account's answer about %s: %w", b.name, err)
	}

	return resp.StatusCode, resp.Header.Get("ETag"), body, nil
}

// refusal turns an unexpected answer into an error carrying the service's own
// code, which is what tells a missing role assignment from a missing container.
func (b *Blob) refusal(doing string, status int, body []byte) error {
	var reason struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}

	if xml.Unmarshal(body, &reason) == nil && reason.Code != "" {
		message, _, _ := strings.Cut(reason.Message, "\n")

		return fmt.Errorf("%s %s: %s: %s (HTTP %d)", doing, b.name, reason.Code, message, status)
	}

	return fmt.Errorf("%s %s: HTTP %d", doing, b.name, status)
}
