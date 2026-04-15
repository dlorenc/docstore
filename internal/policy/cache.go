package policy

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/dlorenc/docstore/internal/store"
)

// ReadStore is the minimal interface for reading policy and OWNERS files.
// *store.Store satisfies this interface.
type ReadStore interface {
	MaterializeTree(ctx context.Context, repo, branch string, atSeq *int64, limit int, afterPath string) ([]store.TreeEntry, error)
	GetFile(ctx context.Context, repo, branch, path string, atSeq *int64) (*store.FileContent, error)
}

// cacheEntry holds a compiled engine and pre-loaded OWNERS map for one repo.
type cacheEntry struct {
	engine *Engine              // nil means no policies (bootstrap mode)
	owners map[string][]string // dir → owner identities
}

// Cache provides thread-safe, per-repository caching of compiled OPA engines
// and OWNERS data. It is lazily populated on first access and invalidated after
// any merge or direct commit to the main branch.
type Cache struct {
	mu      sync.RWMutex
	entries map[string]*cacheEntry
	loaded  map[string]bool // tracks repos that have been loaded (even if engine == nil)
}

// NewCache returns an empty, ready-to-use Cache.
func NewCache() *Cache {
	return &Cache{
		entries: make(map[string]*cacheEntry),
		loaded:  make(map[string]bool),
	}
}

// Load returns the compiled Engine and OWNERS map for the given repo, loading
// them from the read store on first call. Returns nil, nil, nil in bootstrap
// mode (no .rego files on the main branch).
func (c *Cache) Load(ctx context.Context, repo string, readStore ReadStore) (*Engine, map[string][]string, error) {
	// Fast path: already loaded.
	c.mu.RLock()
	if c.loaded[repo] {
		entry := c.entries[repo]
		c.mu.RUnlock()
		if entry == nil {
			return nil, nil, nil
		}
		return entry.engine, entry.owners, nil
	}
	c.mu.RUnlock()

	// Slow path: load from store.
	entry, err := loadEntry(ctx, repo, readStore)
	if err != nil {
		return nil, nil, err
	}

	c.mu.Lock()
	c.entries[repo] = entry
	c.loaded[repo] = true
	c.mu.Unlock()

	if entry == nil {
		return nil, nil, nil
	}
	return entry.engine, entry.owners, nil
}

// Invalidate removes the cached data for the given repo so the next Load
// fetches fresh policy files and OWNERS from the store.
func (c *Cache) Invalidate(repo string) {
	c.mu.Lock()
	delete(c.entries, repo)
	delete(c.loaded, repo)
	c.mu.Unlock()
}

// loadEntry fetches policy files and OWNERS from the main branch of a repo.
func loadEntry(ctx context.Context, repo string, readStore ReadStore) (*cacheEntry, error) {
	modules, err := loadPolicyModules(ctx, repo, readStore)
	if err != nil {
		return nil, fmt.Errorf("load policy modules: %w", err)
	}

	ownerFiles, err := loadOwnerFiles(ctx, repo, readStore)
	if err != nil {
		return nil, fmt.Errorf("load OWNERS files: %w", err)
	}

	if len(modules) == 0 && len(ownerFiles) == 0 {
		return nil, nil // bootstrap mode
	}

	engine, err := NewEngine(ctx, modules)
	if err != nil {
		return nil, fmt.Errorf("compile policies: %w", err)
	}

	return &cacheEntry{
		engine: engine,
		owners: ParseOwners(ownerFiles),
	}, nil
}

// loadPolicyModules fetches all .rego files from .docstore/policy/ on main.
func loadPolicyModules(ctx context.Context, repo string, readStore ReadStore) (map[string]string, error) {
	const policyDir = ".docstore/policy/"
	modules := make(map[string]string)

	// Use afterPath=".docstore/policy" to start from the policy directory.
	// Limit 100 covers any realistic number of policy files.
	entries, err := readStore.MaterializeTree(ctx, repo, "main", nil, 100, ".docstore/policy")
	if err != nil {
		return nil, err
	}

	for _, e := range entries {
		if !strings.HasPrefix(e.Path, policyDir) {
			break // sorted order: we've passed the directory
		}
		if !strings.HasSuffix(e.Path, ".rego") {
			continue
		}
		fc, err := readStore.GetFile(ctx, repo, "main", e.Path, nil)
		if err != nil {
			return nil, fmt.Errorf("read %q: %w", e.Path, err)
		}
		if fc == nil {
			continue
		}
		modules[e.Path] = string(fc.Content)
	}

	return modules, nil
}

// loadOwnerFiles fetches all OWNERS files from the main branch.
func loadOwnerFiles(ctx context.Context, repo string, readStore ReadStore) (map[string][]byte, error) {
	result := make(map[string][]byte)
	afterPath := ""

	for {
		entries, err := readStore.MaterializeTree(ctx, repo, "main", nil, 200, afterPath)
		if err != nil {
			return nil, err
		}
		if len(entries) == 0 {
			break
		}

		for _, e := range entries {
			if e.Path == "OWNERS" || strings.HasSuffix(e.Path, "/OWNERS") {
				fc, err := readStore.GetFile(ctx, repo, "main", e.Path, nil)
				if err != nil {
					return nil, fmt.Errorf("read OWNERS %q: %w", e.Path, err)
				}
				if fc != nil {
					result[e.Path] = fc.Content
				}
			}
			afterPath = e.Path
		}

		if len(entries) < 200 {
			break
		}
	}

	return result, nil
}
