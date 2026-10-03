package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Reloader owns the live configuration. Readers call Current (a lock-free
// atomic load) on every request; Reload swaps in a new validated config.
// An invalid file never replaces a working config.
type Reloader struct {
	path     string
	log      *slog.Logger
	onResult func(ok bool)

	cur atomic.Pointer[Config]

	mu      sync.Mutex // serialises Reload (SIGHUP and file polling can race)
	lastSum [sha256.Size]byte
}

// NewReloader loads path once and returns a Reloader serving it. onResult
// (may be nil) is told whether each subsequent reload succeeded.
func NewReloader(path string, log *slog.Logger, onResult func(ok bool)) (*Reloader, error) {
	cfg, raw, err := Load(path)
	if err != nil {
		return nil, err
	}
	if onResult == nil {
		onResult = func(bool) {}
	}
	r := &Reloader{path: path, log: log, onResult: onResult, lastSum: sha256.Sum256(raw)}
	r.cur.Store(cfg)
	return r, nil
}

// Current returns the live configuration. Callers must not mutate it.
func (r *Reloader) Current() *Config { return r.cur.Load() }

// Reload re-reads the file and applies its hot-reloadable settings.
// It reports whether a new configuration was applied.
func (r *Reloader) Reload() (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reloadLocked(false)
}

func (r *Reloader) reloadLocked(onlyIfChanged bool) (bool, error) {
	raw, err := os.ReadFile(r.path)
	if err != nil {
		r.fail(err)
		return false, err
	}
	sum := sha256.Sum256(raw)
	if onlyIfChanged && bytes.Equal(sum[:], r.lastSum[:]) {
		return false, nil
	}
	next, err := Parse(raw)
	if err != nil {
		// Remember the bad content so polling doesn't log the same error every tick.
		r.lastSum = sum
		r.fail(err)
		return false, err
	}
	r.lastSum = sum
	cur := r.cur.Load()
	if diff := cur.StaticDiff(next); len(diff) > 0 {
		r.log.Warn("config changes that need a restart were ignored", "fields", diff)
	}
	r.cur.Store(cur.WithReloadable(next))
	r.log.Info("config reloaded", "path", r.path, "routes", len(next.Routes))
	r.onResult(true)
	return true, nil
}

func (r *Reloader) fail(err error) {
	r.log.Error("config reload failed; keeping previous config", "path", r.path, "err", err)
	r.onResult(false)
}

// Watch polls the file every interval and reloads when its content changes.
// Polling (rather than inotify) also handles Kubernetes ConfigMap volumes,
// which update by atomically swapping a symlink.
func (r *Reloader) Watch(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.mu.Lock()
			_, _ = r.reloadLocked(true)
			r.mu.Unlock()
		}
	}
}
