// The model catalog: what each agent says it can run.
//
// The catalog is deliberately advisory. Nothing validates a mission's model
// against it, and a daemon with no probers still launches missions — they simply
// run on whatever their agent defaults to. That is the same position q occupied
// before missions could carry a model at all, which is what makes a failed probe
// a degraded board rather than a broken one.

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/paths"
)

// DefaultModelRefresh is how often the daemon re-asks each agent what it offers.
//
// It is hours rather than minutes because the answer changes when a subscription
// or a release does, not while anyone is looking, and because probing claude
// means starting it, which fires the user's own SessionStart hooks. Anyone who
// wants a fresher answer sooner can ask for one.
const DefaultModelRefresh = 6 * time.Hour

// modelCacheFile is where the last answer is kept, so a daemon that has just
// restarted serves the models it knew rather than an empty form.
const modelCacheFile = "models.json"

// WithProber attaches an agent that can be asked which models it offers.
func WithProber(p mission.ModelProber) Option {
	return func(s *Service) { s.probers = append(s.probers, p) }
}

// WithModelRefresh overrides how often the catalog is refreshed.
func WithModelRefresh(d time.Duration) Option {
	return func(s *Service) { s.modelRefresh = d }
}

// catalog holds what each agent last said it offers.
type catalog struct {
	mu   sync.RWMutex
	sets map[mission.Tool]mission.ModelSet
}

// newCatalog returns an empty catalog.
func newCatalog() *catalog {
	return &catalog{sets: map[mission.Tool]mission.ModelSet{}}
}

// all returns a copy of every known set.
func (c *catalog) all() map[mission.Tool]mission.ModelSet {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return maps.Clone(c.sets)
}

// get returns one agent's set.
func (c *catalog) get(tool mission.Tool) mission.ModelSet {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.sets[tool]
}

// put records an agent's answer.
func (c *catalog) put(tool mission.Tool, set mission.ModelSet) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.sets[tool] = set
}

// fail records a failed probe against whatever is already known.
//
// The previous answer is kept rather than cleared: a stale list of real models
// is more useful than no list, and the recorded error is what tells the board
// and q doctor that it is stale.
func (c *catalog) fail(tool mission.Tool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	set := c.sets[tool]
	set.Err = err.Error()
	c.sets[tool] = set
}

// replace swaps in a whole catalog, used when loading the disk cache.
func (c *catalog) replace(sets map[mission.Tool]mission.ModelSet) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.sets = sets
}

// Models returns what each agent offers, as of the last probe.
func (s *Service) Models() map[mission.Tool]mission.ModelSet {
	return s.models.all()
}

// ModelsFor returns one agent's models.
func (s *Service) ModelsFor(tool mission.Tool) mission.ModelSet {
	return s.models.get(tool)
}

// RefreshModels asks every agent what it offers and returns the result.
//
// Probes run in sequence rather than in parallel: there are at most a handful of
// agents, each probe starts a real process, and doing them one at a time keeps a
// refresh from being a burst of load whenever the daemon starts.
func (s *Service) RefreshModels(ctx context.Context) map[mission.Tool]mission.ModelSet {
	for _, prober := range s.probers {
		tool := prober.Tool()

		set, err := prober.Probe(ctx)
		if err != nil {
			s.warn("asking an agent for its models", "tool", tool, "error", err)
			s.models.fail(tool, err)

			continue
		}

		s.models.put(tool, set)
	}

	if len(s.probers) > 0 {
		s.saveModelCache()
	}

	return s.Models()
}

// RunModelRefresher keeps the catalog current until ctx is canceled.
//
// The disk cache is loaded first so the board has something to offer during the
// seconds the first probe takes.
func (s *Service) RunModelRefresher(ctx context.Context) {
	s.loadModelCache()

	if len(s.probers) == 0 {
		return
	}

	s.RefreshModels(ctx)

	ticker := time.NewTicker(s.refreshInterval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.RefreshModels(ctx)
		}
	}
}

// refreshInterval is how often to re-probe.
func (s *Service) refreshInterval() time.Duration {
	if s.modelRefresh > 0 {
		return s.modelRefresh
	}

	return DefaultModelRefresh
}

// modelCachePath is where the catalog is kept between runs.
func (s *Service) modelCachePath() string {
	return filepath.Join(s.dirs.State, modelCacheFile)
}

// loadModelCache reads the last known catalog.
//
// Every failure is silent and leaves the catalog empty. The cache is an
// optimization, and a corrupt one is replaced by the next successful probe.
func (s *Service) loadModelCache() {
	data, err := os.ReadFile(s.modelCachePath()) // #nosec G304 -- a path q owns.
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.warn("reading the model cache", "error", err)
		}

		return
	}

	var sets map[mission.Tool]mission.ModelSet
	if err := json.Unmarshal(data, &sets); err != nil {
		s.warn("parsing the model cache", "error", err)

		return
	}

	if len(sets) > 0 {
		s.models.replace(sets)
	}
}

// saveModelCache writes the catalog for the next daemon to start with.
func (s *Service) saveModelCache() {
	data, err := json.MarshalIndent(s.models.all(), "", "  ")
	if err != nil {
		s.warn("encoding the model cache", "error", err)

		return
	}

	if err := writeFileAtomic(s.modelCachePath(), append(data, '\n')); err != nil {
		s.warn("writing the model cache", "error", err)
	}
}

// writeFileAtomic replaces a file through a temporary one in the same directory,
// so a daemon killed mid-write leaves the previous cache intact rather than a
// truncated file the next start would have to reject.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("creating a temporary file in %s: %w", dir, err)
	}

	name := tmp.Name()

	defer func() {
		_ = tmp.Close()
		_ = os.Remove(name)
	}()

	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("writing %s: %w", name, err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", name, err)
	}

	if err := os.Chmod(name, paths.FileMode); err != nil {
		return fmt.Errorf("setting the mode of %s: %w", name, err)
	}

	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("replacing %s: %w", path, err)
	}

	return nil
}
