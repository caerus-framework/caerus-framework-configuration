# Configuration on Kubernetes

This guide explains how `caerus-framework-configuration` behaves with
Kubernetes-mounted configuration and secrets, and how to deploy it safely.

## How Kubernetes mounts config

A ConfigMap or Secret volume mount is a **symlink farm**. The mount directory
contains per-key symlinks that point through a versioned `..data` symlink:

```
/etc/caerus/
├── mongodb.json -> ..data/mongodb.json     (symlink)
├── ..data       -> ..data_2024-01-01T00:00 ->  ..data   (symlink)
└── ..data_2024-01-01T00:00/
    └── mongodb.json                       (real file)
```

When the ConfigMap/Secret changes, Kubernetes **re-points `..data`** to a new
timestamped directory and swaps the per-key symlinks via rename. The real file
inodes are never edited in place — they are replaced.

## Why watching the file is not enough

`fsnotify`/inotify watches an **inode**, not a path. A symlink swap replaces
the directory entry but keeps nothing about the old inode, so a watcher
attached to `mongodb.json`:

- receives the rename/create event for the path, but
- re-opening the **old inode** would read stale content (and on the next swap,
  the inode is gone entirely).

## What the configuration component does instead

1. **Watch the parent directory**, not the file. Directory watches receive
   rename/create/write events for their children, which covers both in-place
   edits (local dev) and symlink swaps (Kubernetes).
2. **Re-stat the target path** on every relevant event. Because the watch is on
   the directory, the new `..data` symlink is followed and the **new** content
   is read.
3. **Deduplicate by content hash** (sha256). Kubernetes may rewrite files even
   when nothing semantically changed (identical bytes, chmod touches, other
   keys in the same mount updating). The hash check means only real changes
   trigger a reload and a `OnConfigReload()` notification.

Result: a config update on a mounted volume is detected, re-read exactly once,
validated, and swapped atomically.

## Secrets

Credentials on Kubernetes have **two named paths**. They are not the same
mechanism. Prefer Path B for chassis files (postgres, valkey, HTTP bind
config). Path A is live fetch. One app can use **both** (see Mixed).

Never put credentials in a ConfigMap or in the image.

### Path B — ESO mounted file (preferred / golden)

**What it is.** External Secrets (or a native Secret) writes a file on
the volume. This module’s source `Path` points at that file. A symlink
swap is rotation: watch → re-stat → hash → validate → `OnConfigReload`.
The process does not call Vault.

**Who / what.** Operator + ESO own the mount. Configuration owns reload.
The chassis component (`cf_postgres`, …) is `Source.Owner` and rebuilds
its client from the new value.

**Use when.** The secret is part of a typed config blob (`password` on
`PostgresConfig`, `api_key` on mail). That is the ops-oriented golden
path: files are the Kubernetes rotation plane.

```yaml
# Secret (or ExternalSecret targeting a Secret) mounted next to ConfigMaps
spec:
  template:
    spec:
      containers:
        - name: app
          volumeMounts:
            - name: app-config
              mountPath: /etc/caerus
              readOnly: true
      volumes:
        - name: app-config
          projected:
            sources:
              - configMap:
                  name: app-config   # host, port, non-secret settings
              - secret:
                  name: app-db       # postgresql.json with password
```

Point the source at the mounted file (`/etc/caerus/postgresql.json`).
Tag the password `secret:"redact"` so reload logs use `LogArgs`, not
`slog.Any`.

### Path A — in-process Get (`caerus-framework-secrets`)

**What it is.** The published
[`caerus-framework-secrets`](https://github.com/caerus-framework/caerus-framework-secrets)
component. `main` `AddComponent`s it. Callers store the **component
pointer** and call `Get` / `GetString` per use (Vault, OpenBao, AWS,
GCP, or that module’s file driver). Kind stays in *that* module’s
config. Rotation is a later `Get`, not this module’s `fsnotify`.

**What it is not.** Core `SecretsStage` is a reserved empty slot. Do not
wait for a bootstrap secrets component inside `caerus-framework`. The
public module is the thing you register.

**Use when.** The app needs a live fetch (short-lived token, many keys,
a backend that is not “one JSON file per chassis source”).

### Mixed in one app

Path B and Path A can run in the **same process**. Typical split:

- **Path B:** postgres / valkey / HTTP settings — ESO file → this
  module → chassis `OnConfigReload`.
- **Path A:** a third-party API token or mTLS material the app fetches
  through `cf_secrets.Get`.

Do not put the Vault token into `postgresql.json`, and do not `Get` the
postgres password from Vault if that password already rotates as a
mounted file. Two planes, two owners; mixing *inside one credential*
(file *and* Vault for the same password) is how juniors lose the plot.

## Recommended deployment

```yaml
# configmap.yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
data:
  mongodb.json: |
    {"host": "mongo.internal", "port": 27017}
```

```yaml
# deployment.yaml
spec:
  template:
    spec:
      containers:
        - name: app
          image: ghcr.io/yourorg/app:1.0
          volumeMounts:
            - name: app-config
              mountPath: /etc/caerus
              readOnly: true
      volumes:
        - name: app-config
          configMap:
            name: app-config
```

```go
path := "/etc/caerus/mongodb.json" // path resolves through ..data on reload
if err := cf_configuration.AddSource(fwCfg, cf_configuration.Source[MongoConfig]{
    Name: "mongodb", Path: path, Format: cf_configuration.FormatJSON,
}); err != nil { /* fail fast at startup */ }
```

## Operational notes

- **Startup**: `AddSource` registers even when the construct Path is missing so
  `--<name>` can point at the real mount. After `ParseFlags`, the resolved file
  must exist (bad parse / validation still fail). Prefer matching mount and
  construct Path when you can; path flags are the escape hatch for Helm.
  If the mount may legitimately be late, register the source only after it is
  known to exist, or add an explicit readiness check before `fw.Run`.
- **File size**: each source file must be **1 MiB or smaller**
  (`MaxConfigFileBytes`). That is also the ConfigMap/Secret object size, so a
  normal mount already cannot exceed it. A bigger file (wrong `--<name>`
  path, a log dump bind-mounted over the config) fails the load; a reload
  keeps last-good. The cap is the file on disk, not YAML decode memory.
  Prefer **JSON** in production mounts. YAML can expand in RAM beyond the
  1 MiB on-disk cap; this module does not budget decode memory.
- **Trusted path**: `Path` and `--<name>` are operator / Pod-spec input.
  The process will open any file that uid can read (Unix DAC). There is no
  directory allowlist. Production is a **mounted** file under a directory
  you chose; reload of that mount **is** rotation. Do not pass untrusted
  argv as `--postgresql=/host/secret`. `filepath.Abs` is not a sandbox.
  See the README → Trusted paths.
- **Rollouts**: `kubectl rollout restart` is still the safe way to apply a
  change if you want a clean process start; hot-reload is for changes that must
  not interrupt service.
- **Large ConfigMaps**: every file in the mount shares the watched directory.
  Events for unrelated keys are cheap (hash dedup) but will cause a re-read of
  the file; keep per-source directories small if you are extremely sensitive to
  I/O.
