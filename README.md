# caerus-framework-configuration

[![CI](https://github.com/caerus-framework/caerus-framework-configuration/actions/workflows/ci.yml/badge.svg)](https://github.com/caerus-framework/caerus-framework-configuration/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/caerus-framework/caerus-framework-configuration/graph/badge.svg)](https://codecov.io/gh/caerus-framework/caerus-framework-configuration)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)


Caerus Framework — configuration component.

Per-component, strongly-typed configuration with validated hot-reload. Each
component registers its own config **source** and reads the current value
through a typed accessor. Load order (later wins):

```text
code zero value → file (optional) → env overlay (EnvPrefix)
→ flag overlay (flag tags) → AfterLoad → Validate
```

Files are the Kubernetes rotation plane (External Secrets → mount →
`fsnotify`). Env overlays support local/CI/PaaS; fileless sources are allowed
when `EnvPrefix` is set. Flags are a **process-start overlay** for the same
fields (`ParseFlags`) — not a second config system; unknown flags and
positional args are returned so subcommands survive. Values are swapped
**atomically** after validation; rejected reloads keep the previous value.

## Features

- **Per-component sources**: every source is generic over its config type —
  no shared global config struct, no `map[string]interface{}` reads.
- **Fail-fast startup**: `AddSource` builds + validates immediately and
  returns the error; a broken config never starts.
- **Env overlay**: `EnvPrefix` maps `PREFIX` + `env`/`json` field names onto
  the struct after the file decode (or onto a zero value when `Path` is empty).
- **Flag overlay**: `flag`-tagged fields get a `--<flag>` via `ParseFlags`,
  applied after env and before `AfterLoad`. Long names only (stdlib `flag`).
- **AfterLoad**: hook for DSN/URL overlays (e.g. `POSTGRES_DSN`, `VALKEY_URL`)
  before Validate.
- **Validated hot-reload**: `fsnotify` watches the file's directory. On change
  the file is re-read, env/AfterLoad reapplied, re-validated, then swapped.
- **Forced reload**: `Reload(name)` / `ReloadAll()` re-apply env+AfterLoad even
  when file bytes are unchanged (see “Detecting env changes” below).
- **Reload dispatch**: after a validated swap, the source's owner component
  (by name) gets `OnConfigReload(source, cfg)` with the **fresh value** if it
  implements `cf.ConfigReloader`. The owner is also notified **once at `Init`**
  with the source's initial value, and immediately from `AddSource` when the
  component is already initialized — so logs/observability (which cannot import
  this module) boot on defaults and then receive their real config.
- **Kubernetes-safe**: watches the parent directory and re-stats the target, so
  ConfigMap/Secret **symlink swaps** are detected; identical content is
  deduplicated by hash (no spurious reloads). See
  [docs/K8S.md](docs/K8S.md).

## Usage

Configuration is **always-on core**: `cf.New(&cf.FrameworkOptions{…})` registers
logs → configuration → observability. `main` does **not** construct
`cf_configuration.New()` or call `ParseFlags` — components own their sources
(`WithConfigSource` / `cf.ConfigSourceRegistrar`), and the framework absorbs
argv (registrar pass → `ParseFlags`) before `Initialize` / `Run`.

Golden path (same shape as
[`caerus-framework-demoapp`](https://github.com/caerus-framework/caerus-framework-demoapp)):

```go
package main

import (
	"context"
	"log"
	"time"

	cf "github.com/caerus-framework/caerus-framework"
	cf_postgres "github.com/caerus-framework/caerus-framework-postgresql"
	cf_valkey "github.com/caerus-framework/caerus-framework-valkey"

	"example.com/myapp/internal/app"
)

func main() {
	fw := cf.New(&cf.FrameworkOptions{
		Logs: &cf.LogsSettings{
			Format:       "json",
			Level:        "info",
			ConfigSource: "logs", // core Source[LogConfig]; file config/logs.json
		},
		Observability: &cf.ObservabilitySettings{
			Address:      ":9090",
			ConfigSource: "observability",
		},
		Components: []cf.CaerusComponent{
			// Module registers Source[PostgresConfig] itself (name, path, env, job).
			cf_postgres.New(
				cf_postgres.WithConfigSource("postgresql", "config/postgresql.json",
					cf_postgres.WithSourceEnvPrefix("POSTGRES_")),
				// Local only: WithMigrateOnInit(). Prod: --postgresql.job=migrate.
			),
			cf_valkey.New(
				cf_valkey.WithConfigSource("valkey", "config/valkey.json"),
			),
			app.New(app.Options{}), // may register a "demoapp" / app source the same way
		},
	})

	if err := fw.RunWithSignals(context.Background(),
		cf.WithShutdownTimeout(15*time.Second),
	); err != nil {
		log.Fatal(err)
	}
}
```

What the module does under `WithConfigSource` (you normally do **not** call this
from `main` — stock chassis already do):

```go
// Inside RegisterConfigSources / Init-time registrar (owner = c.Name()):
_ = cf_configuration.AddSource(cfg, cf_configuration.Source[cf_postgres.PostgresConfig]{
	Name:      "postgresql",
	Path:      "config/postgresql.json", // K8s-mounted file / symlink OK
	Format:    cf_configuration.FormatJSON,
	Owner:     c.Name(),
	EnvPrefix: "POSTGRES_",
	Job:       cf.JobSpec{Flag: "postgresql.job", Tasks: []string{"migrate"}},
	AfterLoad: func(c *cf_postgres.PostgresConfig) error {
		if dsn := os.Getenv("POSTGRES_DSN"); dsn != "" {
			return cf_postgres.OverlayDSN(c, dsn)
		}
		return nil
	},
	Validate: func(v *cf_postgres.PostgresConfig) error { /* … */ return nil },
})
```

Read the current value after the configuration stage has initialized (prefer
`Lookup` / `Get` so a missing source is an error, not a panic):

```go
func (c *CFPostgres) Init(ctx context.Context, fw *cf.CaerusFramework) error {
	cfg, err := cf_configuration.Lookup[cf_postgres.PostgresConfig](fwCfg, c.configSource)
	if err != nil {
		return err
	}
	_ = cfg
	return nil
}
```

Receive reloads by implementing `cf.ConfigReloader` on the **owner**. The
configuration component delivers the **fresh value** as `cfg any` (type-assert
it) plus the `source` name:

```go
func (c *CFPostgres) OnConfigReload(source string, cfg any) {
	if source != c.configSource {
		return
	}
	typed, ok := cfg.(*cf_postgres.PostgresConfig)
	if !ok {
		c.logger.Error("cf_postgres: config reload rejected", "source", source)
		return
	}
	// build new pool → ping → swap under mutex → close old (last-good on failure)
	_ = typed
}
```

**Note**: Prefer `Lookup` (or `Get`) in Init/reload so misconfiguration returns
an error. `MustGet` panics on a missing source — fine for `main`/tests, not for
reload paths. It is safe to call `Get` / `Lookup` / `MustGet` from within
`OnConfigReload`: the configuration component releases its internal lock before
invoking the callback, so there is no risk of deadlock.

`AddComponent` / bare `cf.New()` without options remain valid for tests and
embedded use; production services should follow the `FrameworkOptions` shape
above.

## Flag overlay (`ParseFlags`)

Flags are a **process-start overlay** over the exact same fields as env — not a
second config system. A field gets a `--<flag>` (stdlib `flag`, long names only)
when its `flag` struct tag is set; `flag:"-"` opts out, and an absent tag means
no CLI for that field. Bool fields register as bare flags (`--tls`, `--tls=false`);
everything else takes a value (`--host db.internal` or `--host=db.internal`).
Every source with a `Path` additionally gets a `--<Name>` file-path flag
(default = the source's `Path`).

Contract:

- **Register first, parse second:** every `AddSource` must run before
  `ParseFlags` so flag definitions exist. The framework enforces that order
  (registrars → `ParseFlags`). `ParseFlags` re-loads all sources with the flag
  values applied, and the parsed map is kept and re-applied on every later
  `Reload` / `ReloadAll` (flags do not hot-reload on their own; they are
  process-start only).
- **Unique field flags:** `flag` names are a process-wide namespace across all
  registered sources (including core `logs` / `observability`). The same
  `--short-flag` declared on two sources — or twice on one source — is a
  wiring error at `ParseFlags`, even when types match. Do not reuse short tags
  like `flag:"host"` across modules; pick distinct names (e.g. `log-level`,
  `http-addr`).
- **Per-source file-path flags:** every source with a `Path` also gets a
  `--<Name>` flag (default = its `Path`). Providing it overrides where that
  source's file is read from — the file location is itself a per-source option.
  There is no "config directory" bootstrap knob; each source declares its own
  file, env and arg options.
- **Unknown flags and positional args survive:** the first unknown flag,
  single-dash arg, positional arg, or `--` terminator moves the rest of the
  command line to the returned `rest` untouched — so `serve` / `migrate` /
  app flags fall through to the app.
- **Layering:** flags win over env, env wins over file, `AfterLoad` runs last
  (DSN/URL merges see the final value).
- **Job flags:** a module declares a job on its source with `Source.Job`
  (`cf.JobSpec{Flag, Tasks}`). The flag **names the instance** and the value
  **names the task** to run on it (e.g. `--postgresql.job=migrate`). Jobs are
  **CLI-only** — the value never flows from env or file (the config struct
  carries no job field); `ParseFlags` registers `--<Flag>` as a string flag and
  `JobRequests()` (implements `cf.JobSource`) returns the parsed request(s)
  after argv absorption, validating the task against the declared `Tasks`
  (fail-fast before any data Init). A job flag colliding with a field flag or
  another source's job flag is a parse-time wiring error.

In production binaries **`main` never calls `ParseFlags`**. The framework runs
the registrar pass (every `ConfigSourceRegistrar`) then `ParseFlags` at the
start of `Initialize` / `Run` / `RunWithSignals`. Leftover positionals /
unknown flags are available as `fw.LeftoverArgs()` for the app. See demoapp
`cmd/demoapp/main.go` for the full pattern.

## Detecting env changes (with or without a file)

The process environment is **not** watchable (no inotify on `environ`). With a
file present:

| Trigger | What happens |
|---|---|
| File bytes change (External Secrets, ConfigMap swap) | Automatic: re-read file → re-apply current env → AfterLoad → notify owner |
| Env changes, file unchanged | **Nothing** until something calls `Reload` / `ReloadAll` |
| Fileless source (`EnvPrefix` only) | Same: use `Reload` after env changes |

Recommended patterns:

1. **Kubernetes (preferred):** put rotating secrets in **mounted files**; do not
   rely on env for rotation. File watch is the signal.
2. **Explicit refresh:** call `cfg.Reload("postgresql")` from a SIGHUP handler,
   admin endpoint, or after a known env update in tests.
3. **Do not poll** the environment in a tight loop.

```go
// Example: SIGHUP re-applies env overlays and notifies ConfigReloaders.
go func() {
    for range sighupCh {
        _ = cfg.ReloadAll()
    }
}()
```

## Component contract

Implements `caerusframework.CaerusComponent`:

- `Name()` → `"configuration"` (`cf_configuration.ComponentName`)
- `GetInitOrderStage()` → `caerusframework.ConfigurationStage` (second
  bootstrap stage, right after logs — so later components can read their config
  during `Init`)
- `GetDependencies()` → `[logs]`: the component logs through the framework
  `logs` component; the logger is re-delivered on `logs` `Reconfigure`.
  `WithLogger(*slog.Logger)` overrides the logger for tests/embedded use;
  without a `logs` component the fallback is `slog.Default()`.
- `Init` starts the watcher + reload loop; `Shutdown` stops them cleanly.
- Implements `cf.MetricsProvider`: contributes a `caerus_configuration_info`
  sample (count + source names) to the `observability` component's `/metrics`;
  reports nothing before any source is registered (lazy pickup).

## Hot-reload semantics

| Situation | Behaviour |
|---|---|
| Initial load failure (missing file, bad parse, validation error) | `AddSource` returns an error; source not registered; startup continues to fail via the caller |
| Valid change detected | New value swapped in atomically; owner `OnConfigReload(source, cfg)` called |
| Malformed content on reload | Rejected; previous value kept; error logged |
| Validator rejects new value on reload | Rejected; previous value kept; error logged |
| Content unchanged (e.g. K8s rewrites identical bytes) | Skipped (sha256 dedup); no reload, no notification |
| Multiple configs in one directory | Any event re-checks affected sources; hash dedup keeps it cheap and correct |

## Docs

- [docs/K8S.md](docs/K8S.md) — running on Kubernetes: ConfigMap/Secret mounts,
  symlink swaps, and what the watcher does about them.
- [ARCHITECTURE.md](https://github.com/caerus-framework/caerus-framework/blob/main/docs/ARCHITECTURE.md)
  — component model and stage ordering.

## License

Apache License 2.0 — see [LICENSE](LICENSE).
