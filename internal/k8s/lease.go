// Package k8s keeps a pair's witness claim in a Kubernetes Lease.
//
// A Lease is the object Kubernetes itself uses for this: one small record, a
// holder, a renewal time, and writes that are refused when the record has
// moved on. Any cluster both machines can reach will do, and it needs to grant
// them nothing but get, create and update on leases in one namespace.
//
// The API is spoken directly, as JSON over HTTP. client-go is used for the one
// thing that is hard to get right by hand, which is turning a kubeconfig into
// an authenticated client, and not for a typed clientset that would compile
// every API group into q to read one object.
package k8s

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/justinrush/q/internal/mission"
)

// Defaults for a lease nobody named.
const (
	DefaultNamespace = "default"
	DefaultName      = "q-witness"
)

const (
	leaseAPIVersion = "coordination.k8s.io/v1"
	leaseKind       = "Lease"
	// microTime is the layout of a Kubernetes MicroTime.
	microTime = "2006-01-02T15:04:05.000000Z07:00"
	// bodyLimit bounds how much of a response is read. A Lease is a few
	// hundred bytes, and an error page from a proxy in between need not be.
	bodyLimit = 1 << 20
)

// Config says which Lease to use and how to reach its cluster.
type Config struct {
	// Kubeconfig is the file to load, empty for the usual search: $KUBECONFIG,
	// then ~/.kube/config, then the service account of the pod q runs in.
	Kubeconfig string
	// Context is the kubeconfig context, empty for its current one.
	Context string
	// Namespace and Name locate the Lease. Both hosts of a pair must agree on
	// them, whatever they each call the cluster.
	Namespace string
	Name      string
}

// Lease is a witness backed by one Lease object.
type Lease struct {
	client    *http.Client
	url       string
	namespace string
	name      string
}

// New returns the Lease the configuration names, reached with the credentials
// its kubeconfig supplies. Nothing is contacted until the first read.
func New(cfg Config) (*Lease, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = cfg.Kubeconfig

	restConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{CurrentContext: cfg.Context},
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("loading the kubeconfig: %w", err)
	}

	client, err := rest.HTTPClientFor(restConfig)
	if err != nil {
		return nil, fmt.Errorf("building a client for %s: %w", restConfig.Host, err)
	}

	server, _, err := rest.DefaultServerUrlFor(restConfig)
	if err != nil {
		return nil, fmt.Errorf("resolving the cluster address %q: %w", restConfig.Host, err)
	}

	return NewAt(client, server.String(), cfg.Namespace, cfg.Name), nil
}

// NewAt returns a Lease on the API server at the given address, reached with
// the given client. An empty namespace or name takes the default.
func NewAt(client *http.Client, server, namespace, name string) *Lease {
	if namespace == "" {
		namespace = DefaultNamespace
	}

	if name == "" {
		name = DefaultName
	}

	return &Lease{
		client:    client,
		namespace: namespace,
		name:      name,
		url: strings.TrimRight(server, "/") + "/apis/coordination.k8s.io/v1/namespaces/" +
			url.PathEscape(namespace) + "/leases",
	}
}

// Name identifies the Lease by namespace and name. The cluster is left out on
// purpose: two machines routinely reach one cluster by different addresses.
func (l *Lease) Name() string { return "kubernetes:" + l.namespace + "/" + l.name }

// lease is the part of a coordination.k8s.io/v1 Lease that q reads and writes.
type lease struct {
	APIVersion string    `json:"apiVersion"`
	Kind       string    `json:"kind"`
	Metadata   metadata  `json:"metadata"`
	Spec       leaseSpec `json:"spec"`
}

type metadata struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace"`
	ResourceVersion string            `json:"resourceVersion,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
}

type leaseSpec struct {
	HolderIdentity       string `json:"holderIdentity,omitempty"`
	LeaseDurationSeconds *int32 `json:"leaseDurationSeconds,omitempty"`
	RenewTime            string `json:"renewTime,omitempty"`
}

// Read returns the claim the Lease holds, the zero Claim when it does not exist.
func (l *Lease) Read(ctx context.Context) (mission.Claim, error) {
	status, body, err := l.do(ctx, http.MethodGet, l.url+"/"+url.PathEscape(l.name), nil)
	if err != nil {
		return mission.Claim{}, err
	}

	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		return mission.Claim{}, nil
	default:
		return mission.Claim{}, l.refusal("reading", status, body)
	}

	var stored lease
	if err := json.Unmarshal(body, &stored); err != nil {
		return mission.Claim{}, fmt.Errorf("decoding lease %s: %w", l.Name(), err)
	}

	claim := mission.Claim{
		Holder:  mission.HostID(stored.Spec.HolderIdentity),
		Version: stored.Metadata.ResourceVersion,
	}

	if stored.Spec.LeaseDurationSeconds != nil {
		claim.TTL = time.Duration(*stored.Spec.LeaseDurationSeconds) * time.Second
	}

	if stored.Spec.RenewTime != "" {
		claim.RenewedAt, err = time.Parse(time.RFC3339Nano, stored.Spec.RenewTime)
		if err != nil {
			return mission.Claim{}, fmt.Errorf("lease %s has renewTime %q: %w", l.Name(), stored.Spec.RenewTime, err)
		}
	}

	return claim, nil
}

// Write replaces the Lease, creating it when the claim carries no version.
// Either way the API server refuses a write that is not against the latest
// revision, which is reported as [mission.ErrClaimRaced].
func (l *Lease) Write(ctx context.Context, claim mission.Claim) error {
	stored := lease{
		APIVersion: leaseAPIVersion,
		Kind:       leaseKind,
		Metadata: metadata{
			Name:            l.name,
			Namespace:       l.namespace,
			ResourceVersion: claim.Version,
			Labels:          map[string]string{"app.kubernetes.io/managed-by": "q"},
		},
		Spec: leaseSpec{HolderIdentity: string(claim.Holder)},
	}

	if !claim.RenewedAt.IsZero() {
		stored.Spec.RenewTime = claim.RenewedAt.UTC().Format(microTime)
	}

	// A Lease's duration is whole seconds and must be positive when present.
	// Rounding up keeps a claim from lapsing sooner than its holder believes.
	if claim.TTL > 0 {
		seconds := int32(min(math.Ceil(claim.TTL.Seconds()), math.MaxInt32))
		stored.Spec.LeaseDurationSeconds = &seconds
	}

	payload, err := json.Marshal(stored)
	if err != nil {
		return fmt.Errorf("encoding lease %s: %w", l.Name(), err)
	}

	method, target := http.MethodPut, l.url+"/"+url.PathEscape(l.name)
	if claim.Version == "" {
		method, target = http.MethodPost, l.url
	}

	status, body, err := l.do(ctx, method, target, payload)
	if err != nil {
		return err
	}

	switch status {
	case http.StatusOK, http.StatusCreated:
		return nil
	case http.StatusConflict:
		return mission.ErrClaimRaced
	default:
		return l.refusal("writing", status, body)
	}
}

// do makes one request and returns the status and body.
func (l *Lease) do(ctx context.Context, method, target string, payload []byte) (int, []byte, error) {
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return 0, nil, err
	}

	req.Header.Set("Accept", "application/json")

	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := l.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("reaching the cluster for lease %s: %w", l.Name(), err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, bodyLimit))
	if err != nil {
		return 0, nil, fmt.Errorf("reading the cluster's answer about lease %s: %w", l.Name(), err)
	}

	return resp.StatusCode, body, nil
}

// refusal turns an unexpected answer into an error that carries the API
// server's own explanation, which for a missing role binding names the verb
// and resource that were denied.
func (l *Lease) refusal(doing string, status int, body []byte) error {
	var reason struct {
		Message string `json:"message"`
	}

	if json.Unmarshal(body, &reason) == nil && reason.Message != "" {
		return fmt.Errorf("%s lease %s: %s (HTTP %d)", doing, l.Name(), reason.Message, status)
	}

	return fmt.Errorf("%s lease %s: HTTP %d", doing, l.Name(), status)
}
