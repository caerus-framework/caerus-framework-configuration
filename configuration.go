package cf_configuration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"

	cf "github.com/caerus-framework/caerus-framework"
	cf_logs "github.com/caerus-framework/caerus-framework-logs"
	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"
)

// ComponentName is the framework component name for the configuration
// component. It is the identifier other components use in GetDependencies to
// require configuration.
const ComponentName = "configuration"

// MaxConfigFileBytes is the largest configuration file this component will
// read (1 MiB). That matches the Kubernetes ConfigMap/Secret object size.
// Typed chassis configs are far smaller; a bigger file usually means the
// process was pointed at the wrong path. The bound is on-disk size before
// decode — it does not cap YAML expansion in memory (prefer JSON in
// production).
const MaxConfigFileBytes int64 = 1 << 20

// Format selects the on-disk encoding of a configuration file.
type Format int

const (
	// FormatJSON parses JSON files with encoding/json.
	FormatJSON Format = iota
	// FormatYAML parses YAML files with gopkg.in/yaml.v3.
	FormatYAML
)

// Source describes one configuration source and how to interpret it. It is
// generic over the concrete config type, so each component gets its own
// strongly-typed config with no shared global struct.
//
// Load order (later wins): file (if Path set) → env overlay (if EnvPrefix set)
// → flag overlay (if ParseFlags ran and the struct has flag tags) → AfterLoad
// → Validate. Files are the Kubernetes rotation plane (External Secrets →
// mount → fsnotify); env is for local/CI/PaaS; flags are a process-start
// overlay and do not hot-reload by themselves.
type Source[T any] struct {
	// Name is the logical name of this source (e.g. "mongodb"). It is the key
	// used by Get/MustGet and must be unique within the framework.
	Name string
	// Path is the configuration file path. It may be a symlink (as with
	// Kubernetes ConfigMap/Secret mounts), so the directory is watched and the
	// target is re-stat'ed on every event; see docs/K8S.md. Empty is allowed
	// when EnvPrefix is set (fileless / env-only source).
	//
	// A source with a Path also gets a --<Name> file-path flag in ParseFlags:
	// providing it overrides where the file is read from (defaults to this
	// Path). There is no "config directory" bootstrap setting — each source
	// declares its own file, env and arg options.
	//
	// Path is trusted: the process opens any file that uid can read. There is
	// no directory allowlist. The --<Name> override is the same trust (argv /
	// Pod spec), not a sandbox. filepath.Abs is not a jail.
	Path string
	// Format selects the file encoding. Ignored when Path is empty.
	Format Format
	// Owner is the Name of the component that consumes this configuration. On a
	// validated reload, the owner's OnConfigReload (if it implements
	// cf.ConfigReloader) is invoked. Empty disables reload dispatch.
	Owner string
	// EnvPrefix, when non-empty, overlays matching environment variables onto
	// the decoded value after the file is read (or onto a zero value when Path
	// is empty). Keys are EnvPrefix + `env` tag, or UPPER_SNAKE of the json
	// name. Example: EnvPrefix "POSTGRES_" and field `Host` → POSTGRES_HOST.
	EnvPrefix string
	// Job, when declared, registers a CLI-only job flag for this source's Owner.
	// The flag names the instance and the value names the task to run on it
	// (e.g. --postgresql.job=migrate); the framework reads the request via
	// cf.JobSource after argv absorption and routes it before serving. CLI-only:
	// the value lives in the parsed flag, never in the config file or
	// environment. Tasks (if non-empty) restricts the accepted task values;
	// a value outside the set fails JobRequests. Two sources must not set a
	// job flag for the same Owner in one process (JobRequests fails closed:
	// one job per target). The source must set Owner.
	Job cf.JobSpec
	// AfterLoad runs after file+env overlay and before Validate. Use it for
	// DSN/URL overlays (e.g. POSTGRES_DSN → OverlayDSN). Nil skips the step.
	AfterLoad func(*T) error
	// Validate runs after every successful load (initial and reload). It must
	// return nil for the new value to be accepted. On reload, a rejected value
	// keeps the previous one in effect.
	Validate func(*T) error
}

// Option configures the configuration component at construction time.
type Option func(*options)

type options struct {
	logger    *slog.Logger
	loggerSet bool // true when WithLogger was called explicitly
}

// WithLogger overrides the logger used for reload/watcher diagnostics. By
// default the component logs through the framework logs component (declared in
// GetDependencies); WithLogger is an explicit override for tests and embedded
// use and wins over the framework logger. slog.Default() remains the fallback
// only when neither is available.
func WithLogger(logger *slog.Logger) Option {
	return func(o *options) { o.logger = logger; o.loggerSet = true }
}

// Configuration is the caerus-framework-configuration component. It owns a set
// of per-component configuration sources: each is loaded exactly once, watched
// for changes, and swapped atomically on a validated reload.
type Configuration struct {
	mu        sync.Mutex
	fw        *cf.CaerusFramework
	logger    *slog.Logger
	loggerSet bool
	logsSub   *cf_logs.Subscription
	sources   map[string]*source
	// order tracks source registration order so flag/job collision messages and
	// other source-iterating paths stay deterministic (map iteration is not).
	order []string

	// flagValues holds the process-start flag overlay (name → raw string) from
	// the most recent ParseFlags. Guarded by mu; applied by loadSource on every
	// load so flags survive Reload / ReloadAll. Empty until ParseFlags runs.
	flagValues map[string]string

	ctx     context.Context
	cancel  context.CancelFunc
	watcher *fsnotify.Watcher
	// watchedDirs holds a reference count for every fsnotify directory we
	// currently watch. Multiple sources may share the same directory.
	watchedDirs map[string]int
	done        chan struct{}
	once        sync.Once
}

// source is the runtime representation of a Source. The decode and validate
// functions are closures over the source's concrete type T.
type source struct {
	name      string
	path      string // empty = fileless (env / AfterLoad only)
	owner     string
	envPrefix string
	job       cf.JobSpec                // declared job flag (empty Flag = no job declared)
	decode    func([]byte) (any, error) // nil when fileless
	zero      func() any                // returns *T
	after     func(any) error
	validate  func(any) error
	value     atomic.Value // holds the current *T
	hash      string       // sha256 of last accepted file bytes; "env" when fileless
}

// notifyTarget couples a reloaded source's owner with the fresh value to
// deliver. Notification must happen outside the configuration lock: owners may
// call Get/Lookup/MustGet from OnConfigReload.
type notifyTarget struct {
	rel  cf.ConfigReloader
	name string
	cfg  any
}

// New creates a configuration component. Add sources with AddSource (from any
// component's Init) and read the current value with Get/MustGet.
func New(opts ...Option) *Configuration {
	o := options{logger: slog.Default()}
	for _, opt := range opts {
		opt(&o)
	}
	return &Configuration{
		logger:      o.logger,
		loggerSet:   o.loggerSet,
		sources:     make(map[string]*source),
		watchedDirs: make(map[string]int),
	}
}

// Name implements cf.CaerusComponent.
func (c *Configuration) Name() string { return ComponentName }

// GetInitOrderStage implements cf.CaerusComponent. Configuration runs in the
// second bootstrap stage, right after logs, so later components can read their
// config during Init.
func (c *Configuration) GetInitOrderStage() cf.Stage { return cf.ConfigurationStage }

// GetDependencies implements cf.Dependencies. The component logs through the
// framework logs component.
func (c *Configuration) GetDependencies() []string {
	return []string{cf_logs.ComponentName}
}

// Init implements cf.CaerusComponent. It starts the file watcher and the reload
// loop, and begins watching every source registered so far. Sources registered
// later (during other components' Init) are watched immediately.
//
// After starting the watcher it notifies each source's owner with the
// already-loaded value. The core components initialize before configuration
// (logs) or without a Lookup path (logs again) — the initial notification is
// how they receive their configuration after booting on defaults.
func (c *Configuration) Init(ctx context.Context, fw *cf.CaerusFramework) error {
	c.mu.Lock()
	if c.watcher != nil {
		c.mu.Unlock()
		return nil // already initialized
	}
	c.fw = fw
	if !c.loggerSet {
		if logs, ok := cf.Get[*cf_logs.Logs](fw); ok {
			c.logsSub = logs.OnReconfigureFor(c.Name(), func(l *slog.Logger) { c.logger = l })
		}
	}

	c.ctx, c.cancel = context.WithCancel(context.Background())
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		c.mu.Unlock()
		return fmt.Errorf("cf_configuration: create watcher: %w", err)
	}
	c.watcher = watcher
	c.done = make(chan struct{})

	var targets []notifyTarget
	for _, s := range c.sources {
		if s.path != "" {
			if err := c.watchLocked(s); err != nil {
				c.logger.Warn("cf_configuration: cannot watch config source", "source", s.name, "path", s.path, "err", err)
			}
		}
		if rel := c.ownerReloader(s); rel != nil {
			targets = append(targets, notifyTarget{rel: rel, name: s.name, cfg: s.value.Load()})
		}
	}
	c.mu.Unlock()
	go c.watchLoop()

	// Initial notification: deliver the initial value to each owner so core
	// components (logs) and any owner that did not run yet apply it. Owners
	// that are not initialized ignore it (their Init reads the value itself).
	for _, t := range targets {
		t.rel.OnConfigReload(t.name, t.cfg)
	}
	return nil
}

// Shutdown implements cf.CaerusComponent. It stops the watcher and waits for
// the reload loop to exit. Safe to call even if Init never ran.
func (c *Configuration) Shutdown(ctx context.Context) error {
	c.mu.Lock()
	if c.logsSub != nil {
		c.logsSub.Unsubscribe()
		c.logsSub = nil
	}
	cancel := c.cancel
	c.mu.Unlock()
	if cancel == nil {
		return nil
	}
	var err error
	c.once.Do(func() {
		cancel()
		c.mu.Lock()
		if c.watcher != nil {
			err = c.watcher.Close()
		}
		done := c.done
		c.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			err = ctx.Err()
		}
	})
	return err
}

// AddSource registers and loads a configuration source on the given component.
// The value is built immediately (fail-fast); the source is not registered on
// failure. Reloads never fail the process: a rejected reload keeps the previous
// value. AddSource is safe to call before or after Init.
//
// Path and/or EnvPrefix must be set. Format is required when Path is set.
func AddSource[T any](c *Configuration, src Source[T]) error {
	if c == nil {
		return errors.New("cf_configuration: AddSource called on a nil component")
	}
	if src.Name == "" {
		return errors.New("cf_configuration: source Name must not be empty")
	}
	if src.Path == "" && src.EnvPrefix == "" {
		return fmt.Errorf("cf_configuration: source %q needs Path and/or EnvPrefix", src.Name)
	}

	s := &source{
		name:      src.Name,
		owner:     src.Owner,
		envPrefix: src.EnvPrefix,
		zero:      func() any { var v T; return &v },
	}
	if err := registerJob(src.Name, src.Owner, src.Job, &s.job); err != nil {
		return err
	}
	if src.Path != "" {
		switch src.Format {
		case FormatJSON, FormatYAML:
		default:
			return fmt.Errorf("cf_configuration: source %q has unknown format %d", src.Name, src.Format)
		}
		abs, err := filepath.Abs(src.Path)
		if err != nil {
			return fmt.Errorf("cf_configuration: source %q: %w", src.Name, err)
		}
		s.path = abs
		format := src.Format
		s.decode = func(data []byte) (any, error) {
			var v T
			if format == FormatYAML {
				if err := yaml.Unmarshal(data, &v); err != nil {
					return nil, err
				}
			} else if err := json.Unmarshal(data, &v); err != nil {
				return nil, err
			}
			return &v, nil
		}
	}
	if src.AfterLoad != nil {
		after := src.AfterLoad
		s.after = func(v any) error { return after(v.(*T)) }
	}
	if src.Validate != nil {
		validate := src.Validate
		s.validate = func(v any) error { return validate(v.(*T)) }
	}

	return c.registerSource(s)
}

// AddSourceValue registers a configuration source from its generic-free
// declaration (cf.ConfigSourceValue). It is the cycle-free entry point for core
// modules (logs, observability) that the configuration module imports: the
// framework hands them the component as cf.ConfigSourceAdder and they call this
// with their own declaration. Sample's dynamic type selects the concrete config
// struct and decoding, exactly as Source[T].T would.
func (c *Configuration) AddSourceValue(src cf.ConfigSourceValue) error {
	if c == nil {
		return errors.New("cf_configuration: AddSourceValue called on a nil component")
	}
	if src.Name == "" {
		return errors.New("cf_configuration: source Name must not be empty")
	}
	if src.Path == "" && src.EnvPrefix == "" {
		return fmt.Errorf("cf_configuration: source %q needs Path and/or EnvPrefix", src.Name)
	}
	if src.Sample == nil {
		return fmt.Errorf("cf_configuration: source %q Sample must not be nil", src.Name)
	}
	typ := reflect.TypeOf(src.Sample)
	format := FormatJSON
	switch src.Format {
	case "", "json":
	case "yaml":
		format = FormatYAML
	default:
		return fmt.Errorf("cf_configuration: source %q has unknown format %q", src.Name, src.Format)
	}

	s := &source{
		name:      src.Name,
		owner:     src.Owner,
		envPrefix: src.EnvPrefix,
		zero:      func() any { return reflect.New(typ).Interface() },
	}
	if err := registerJob(src.Name, src.Owner, src.Job, &s.job); err != nil {
		return err
	}
	if src.Path != "" {
		abs, err := filepath.Abs(src.Path)
		if err != nil {
			return fmt.Errorf("cf_configuration: source %q: %w", src.Name, err)
		}
		s.path = abs
		f := format
		s.decode = func(data []byte) (any, error) {
			v := reflect.New(typ).Interface()
			if f == FormatYAML {
				if err := yaml.Unmarshal(data, v); err != nil {
					return nil, err
				}
			} else if err := json.Unmarshal(data, v); err != nil {
				return nil, err
			}
			return v, nil
		}
	}
	if src.AfterLoad != nil {
		s.after = src.AfterLoad
	}
	if src.Validate != nil {
		s.validate = src.Validate
	}
	return c.registerSource(s)
}

// registerSource runs the shared AddSource tail: fail-fast initial load, source
// registration, watcher attach, and owner notify (when the component is already
// initialized). Callers must build a fully populated *source first.
func (c *Configuration) registerSource(s *source) error {
	// Initial load: fail-fast. The flag overlay is a process-start snapshot
	// (empty unless ParseFlags already ran — register sources before parsing).
	c.mu.Lock()
	flags := c.flagValues
	c.mu.Unlock()
	if _, err := loadSource(s, true, flags); err != nil {
		return fmt.Errorf("cf_configuration: source %q: %w", s.name, err)
	}

	c.mu.Lock()
	if _, exists := c.sources[s.name]; exists {
		c.mu.Unlock()
		return fmt.Errorf("cf_configuration: source %q is already registered", s.name)
	}
	c.sources[s.name] = s
	c.order = append(c.order, s.name)
	if c.watcher != nil && s.path != "" {
		if err := c.watchLocked(s); err != nil {
			c.mu.Unlock()
			return err
		}
	}
	var rel cf.ConfigReloader
	if c.fw != nil {
		rel = c.ownerReloader(s)
	}
	c.mu.Unlock()

	// A source registered after configuration initialized notifies its owner
	// immediately so core components (logs) that cannot Lookup still apply it.
	if rel != nil {
		rel.OnConfigReload(s.name, s.value.Load())
	}
	return nil
}

// Get returns a snapshot copy of the current value of the named source,
// typed as T. It reports false if the source does not exist or was registered
// with a different type. Returning by value means the caller owns a stable
// copy: it cannot accidentally mutate the live config, and the value does not
// go stale when a reload swaps the internal pointer.
func Get[T any](c *Configuration, name string) (T, bool) {
	var zero T
	if c == nil {
		return zero, false
	}
	c.mu.Lock()
	s := c.sources[name]
	c.mu.Unlock()
	if s == nil {
		return zero, false
	}
	v, ok := s.value.Load().(*T)
	if !ok || v == nil {
		return zero, false
	}
	return *v, true
}

// Lookup returns a snapshot copy of the current value of the named source,
// typed as T, or an error if it does not exist or was registered with a
// different type. Prefer Lookup (or Get) from Init and OnConfigReload so
// misconfiguration surfaces as error rather than panic.
func Lookup[T any](c *Configuration, name string) (T, error) {
	v, ok := Get[T](c, name)
	if !ok {
		var zero T
		return zero, fmt.Errorf("cf_configuration: no configuration source %q of type %T", name, zero)
	}
	return v, nil
}

// MustGet returns a snapshot copy of the current value of the named source,
// typed as T, or panics if it does not exist or was registered with a
// different type. Prefer Lookup in Init/reload; MustGet is crash-fast sugar
// for main and tests where a missing source is a programmer error.
func MustGet[T any](c *Configuration, name string) T {
	v, err := Lookup[T](c, name)
	if err != nil {
		panic(err.Error())
	}
	return v
}

// Sources returns the names of registered configuration sources in sorted
// order. Returns nil when no sources are registered.
func (c *Configuration) Sources() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.sources) == 0 {
		return nil
	}
	names := make([]string, 0, len(c.sources))
	for name := range c.sources {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// watchLoop forwards watcher events to the matching sources until the component
// shuts down.
func (c *Configuration) watchLoop() {
	defer close(c.done)
	for {
		select {
		case <-c.ctx.Done():
			return
		case ev, ok := <-c.watcher.Events:
			if !ok {
				return
			}
			c.handleEvent(ev)
		case err, ok := <-c.watcher.Errors:
			if !ok {
				return
			}
			if err != nil {
				c.logger.Error("cf_configuration: watcher error", "err", err)
			}
		}
	}
}

// handleEvent reloads every source that could be affected by the event.
func (c *Configuration) handleEvent(ev fsnotify.Event) {
	// Collect owners to notify outside the lock to avoid deadlock when
	// OnConfigReload calls Get/MustGet.
	var targets []notifyTarget

	c.mu.Lock()
	for _, s := range c.sources {
		if eventMatches(s, ev) {
			if rel := c.reloadLocked(s); rel != nil {
				targets = append(targets, notifyTarget{rel: rel, name: s.name, cfg: s.value.Load()})
			}
		}
	}
	c.mu.Unlock()

	// Notify owners outside the lock.
	for _, t := range targets {
		t.rel.OnConfigReload(t.name, t.cfg)
	}
}

// eventMatches reports whether an event may touch the source. The source's
// directory is watched, so events on any of its children (direct writes,
// renames, or Kubernetes symlink swaps) match; the content-hash check in
// loadSource filters out irrelevant and redundant events. Fileless sources
// never match.
func eventMatches(s *source, ev fsnotify.Event) bool {
	if s.path == "" {
		return false
	}
	dir := filepath.Dir(s.path)
	return ev.Name == s.path || filepath.Dir(ev.Name) == dir
}

// Reload forces a re-load of the named source, reapplying env overlay and
// AfterLoad even when the file bytes are unchanged. Use this after an external
// process env change (for example from a SIGHUP handler). The process
// environment is not watchable; without Reload (or a file change), new env
// values are invisible. Returns an error if the source is unknown or the load
// is rejected (previous value kept). Notifies the owner on success when the
// effective value changed.
func (c *Configuration) Reload(name string) error {
	if c == nil {
		return errors.New("cf_configuration: Reload on nil component")
	}
	c.mu.Lock()
	s := c.sources[name]
	if s == nil {
		c.mu.Unlock()
		return fmt.Errorf("cf_configuration: unknown source %q", name)
	}
	rel, err := c.reloadLockedForce(s, true)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	if rel != nil {
		rel.OnConfigReload(name, s.value.Load())
	}
	return nil
}

// ReloadAll forces Reload on every registered source. Owners are notified after
// all loads complete. The first load error is returned; later sources still run.
func (c *Configuration) ReloadAll() error {
	if c == nil {
		return errors.New("cf_configuration: ReloadAll on nil component")
	}
	c.mu.Lock()
	targets, firstErr := c.reloadAllLocked()
	c.mu.Unlock()
	for _, t := range targets {
		t.rel.OnConfigReload(t.name, t.cfg)
	}
	return firstErr
}

// reloadAllLocked forces a reload of every registered source and collects the
// owners to notify. Callers must hold c.mu; notification must happen outside
// the lock (owners may call Get/Lookup/MustGet).
func (c *Configuration) reloadAllLocked() ([]notifyTarget, error) {
	names := make([]string, 0, len(c.sources))
	for name := range c.sources {
		names = append(names, name)
	}
	sort.Strings(names)
	var targets []notifyTarget
	var firstErr error
	for _, name := range names {
		s := c.sources[name]
		rel, err := c.reloadLockedForce(s, true)
		if err != nil && firstErr == nil {
			firstErr = err
			continue
		}
		if rel != nil {
			targets = append(targets, notifyTarget{rel: rel, name: name, cfg: s.value.Load()})
		}
	}
	return targets, firstErr
}

// reloadLocked re-reads and re-validates a source. On failure the previous
// value stays in effect; on a real change it returns the owner's ConfigReloader
// (if any) so the caller can notify it outside the lock.
func (c *Configuration) reloadLocked(s *source) cf.ConfigReloader {
	rel, err := c.reloadLockedForce(s, false)
	if err != nil {
		c.logger.Error("cf_configuration: reload rejected; keeping previous value",
			"source", s.name, "path", s.path, "err", err)
		return nil
	}
	return rel
}

func (c *Configuration) reloadLockedForce(s *source, force bool) (cf.ConfigReloader, error) {
	changed, err := loadSource(s, force, c.flagValues)
	if err != nil {
		return nil, err
	}
	if !changed {
		return nil, nil
	}
	c.logger.Info("cf_configuration: configuration reloaded", "source", s.name, "path", s.path, "force", force)
	return c.ownerReloader(s), nil
}

// ownerReloader returns the source's owner as a ConfigReloader, or nil if the
// source has no owner or the owner does not implement cf.ConfigReloader.
func (c *Configuration) ownerReloader(s *source) cf.ConfigReloader {
	if s.owner == "" || c.fw == nil {
		return nil
	}
	owner, ok := c.fw.Component(s.owner)
	if !ok {
		return nil
	}
	rel, ok := owner.(cf.ConfigReloader)
	if !ok {
		return nil
	}
	return rel
}

// watchLocked registers a directory watch for the source. Callers must hold
// c.mu. No-op for fileless sources.
func (c *Configuration) watchLocked(s *source) error {
	if s.path == "" {
		return nil
	}
	dir := filepath.Dir(s.path)
	if c.watchedDirs[dir] == 0 {
		if err := c.watcher.Add(dir); err != nil {
			return err
		}
	}
	c.watchedDirs[dir]++
	return nil
}

// unwatchLocked removes a directory watch when the last source stops
// requiring it. Callers must hold c.mu.
func (c *Configuration) unwatchLocked(dir string) error {
	cnt := c.watchedDirs[dir]
	if cnt <= 1 {
		delete(c.watchedDirs, dir)
		return c.watcher.Remove(dir)
	}
	c.watchedDirs[dir] = cnt - 1
	return nil
}

// loadSource builds, overlays, validates and stores a source value. When force
// is false, unchanged file bytes skip the work (env is not re-read). When force
// is true (Reload / initial AddSource), env, flag and AfterLoad always run.
// Fileless sources are not watched; use Reload to pick up env changes.
func loadSource(s *source, force bool, flags map[string]string) (bool, error) {
	var (
		v    any
		hash string
	)
	if s.path != "" {
		data, err := readConfigFile(s.path)
		if err != nil {
			return false, err
		}
		sum := sha256.Sum256(data)
		hash = hex.EncodeToString(sum[:])
		if !force && s.value.Load() != nil && s.hash == hash {
			return false, nil
		}
		decoded, err := s.decode(data)
		if err != nil {
			return false, err
		}
		v = decoded
	} else {
		hash = "env"
		if !force && s.value.Load() != nil {
			return false, nil
		}
		v = s.zero()
	}

	if s.envPrefix != "" {
		if err := applyEnvOverlay(v, s.envPrefix); err != nil {
			return false, err
		}
	}
	if len(flags) > 0 {
		if err := applyFlagOverlay(v, flags); err != nil {
			return false, err
		}
	}
	if s.after != nil {
		if err := s.after(v); err != nil {
			return false, err
		}
	}
	if s.validate != nil {
		if err := s.validate(v); err != nil {
			return false, err
		}
	}

	// For forced reloads with unchanged file hash, still notify if the overlay
	// changed the effective value (e.g. env appeared). Compare via JSON.
	if !force {
		s.value.Store(v)
		s.hash = hash
		return true, nil
	}
	prev := s.value.Load()
	s.value.Store(v)
	s.hash = hash
	if prev == nil {
		return true, nil
	}
	changed, err := valuesDiffer(prev, v)
	if err != nil {
		return true, nil // store succeeded; treat as changed
	}
	return changed, nil
}

// readConfigFile reads a source file, rejecting anything larger than
// MaxConfigFileBytes. Stat fails fast on a huge mount; LimitReader catches a
// file that grows between Stat and Read (or a lying Size).
func readConfigFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() > MaxConfigFileBytes {
		return nil, fmt.Errorf("config file %q is %d bytes (max %d)", path, st.Size(), MaxConfigFileBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxConfigFileBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > MaxConfigFileBytes {
		return nil, fmt.Errorf("config file %q is larger than %d bytes", path, MaxConfigFileBytes)
	}
	return data, nil
}

func valuesDiffer(a, b any) (bool, error) {
	ab, err := json.Marshal(a)
	if err != nil {
		return false, err
	}
	bb, err := json.Marshal(b)
	if err != nil {
		return false, err
	}
	return string(ab) != string(bb), nil
}

var _ cf.CaerusComponent = (*Configuration)(nil)
var _ cf.Dependencies = (*Configuration)(nil)
