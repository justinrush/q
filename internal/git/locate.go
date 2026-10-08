package git

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

// NormalizeURL reduces a git remote to "host/path", so the same repository
// compares equal however each machine happens to have cloned it.
//
// The three spellings in common use all name one repository:
//
//	git@gitlab.com:justinarush/mac.git
//	https://gitlab.com/justinarush/mac.git
//	ssh://git@gitlab.com:22/justinarush/mac
//
// and a laptop set up over ssh must recognize the checkout a VM cloned over
// https. The scheme, the user, the port, and the .git suffix are all transport
// detail, so they are dropped; the host is lowercased because DNS is not case
// sensitive, and the path is kept as written because some forges are.
//
// A remote that is a plain local path is returned unchanged, since there is no
// host to normalize it against.
func NormalizeURL(remote string) string {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return ""
	}

	host, path, ok := splitRemote(remote)
	if !ok {
		return remote
	}

	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")

	return strings.ToLower(host) + "/" + path
}

// splitRemote separates a remote into host and path, reporting false for one
// that names no host.
func splitRemote(remote string) (host, path string, ok bool) {
	if strings.Contains(remote, "://") {
		parsed, err := url.Parse(remote)
		if err != nil || parsed.Hostname() == "" {
			return "", "", false
		}

		return parsed.Hostname(), parsed.Path, true
	}

	// The scp-like form: [user@]host:path. A colon after a slash is part of a
	// path, not this separator.
	head, tail, found := strings.Cut(remote, ":")
	if !found || strings.Contains(head, "/") {
		return "", "", false
	}

	if _, afterUser, hasUser := strings.Cut(head, "@"); hasUser {
		head = afterUser
	}

	return head, tail, head != ""
}

// OriginURL returns the normalized origin of the repository at dir, or empty
// for one that has no origin.
func (g *Client) OriginURL(ctx context.Context, dir string) (string, error) {
	res, err := g.exec(ctx, dir, "config", "--get", "remote.origin.url")
	if err != nil {
		// Exit status 1 is git's way of saying the key is unset, which for a
		// repository with no origin is an answer rather than a failure.
		if res.ExitCode == 1 {
			return "", nil
		}

		return "", fmt.Errorf("reading the origin of %s: %w", dir, err)
	}

	return NormalizeURL(res.Out()), nil
}

// locatorTTL is how long a scan of the repo roots is trusted.
//
// Finding a checkout by its origin means asking git about every candidate, so
// the answer is kept. It is not kept forever because the usual reason a
// repository cannot be found is that it has not been cloned yet, and the fix
// for that should be noticed without restarting the daemon.
const locatorTTL = 2 * time.Minute

// Locator finds this machine's checkout of a repository named by its origin.
//
// It is what lets an operation written on one machine mean something on
// another. The operation records a path, which is only true where it was
// written; the origin is true everywhere, and the same roots a human picks
// repositories from are searched for a checkout that has it.
type Locator struct {
	git  *Client
	opts ScanOptions
	now  func() time.Time

	mu      sync.Mutex
	byURL   map[string]string
	scanned time.Time
}

// NewLocator returns a locator searching the given roots.
func NewLocator(client *Client, opts ScanOptions) *Locator {
	return &Locator{git: client, opts: opts, now: time.Now}
}

// OriginURL returns the normalized origin of the checkout at path.
func (l *Locator) OriginURL(ctx context.Context, path string) (string, error) {
	return l.git.OriginURL(ctx, path)
}

// Locate returns the path of a checkout whose origin is url.
//
// When several checkouts share an origin the shallowest wins, on the same
// reasoning the picker uses: ~/dev/weave is a likelier "weave" than a second
// clone buried inside another project.
func (l *Locator) Locate(ctx context.Context, url string) (string, bool) {
	if url == "" {
		return "", false
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.byURL == nil || l.now().Sub(l.scanned) > locatorTTL {
		l.byURL = l.scan(ctx)
		l.scanned = l.now()
	}

	path, ok := l.byURL[url]

	return path, ok
}

// scan indexes every checkout under the roots by its origin.
func (l *Locator) scan(ctx context.Context) map[string]string {
	found := map[string]string{}

	for _, candidate := range Scan(l.opts) {
		origin, err := l.git.OriginURL(ctx, candidate.Path)
		if err != nil || origin == "" {
			continue
		}

		if existing, ok := found[origin]; ok && levels(existing) <= levels(candidate.Path) {
			continue
		}

		found[origin] = candidate.Path
	}

	return found
}

// levels counts the directories in an absolute path.
func levels(path string) int { return strings.Count(path, "/") }
