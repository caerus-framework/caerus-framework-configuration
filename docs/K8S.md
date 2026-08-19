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

- **Prefer the `secrets` bootstrap stage for credentials.** The configuration
  component handles non-secret configuration and reload semantics. Keep
  passwords/tokens/mTLS material in a `caerus-framework-secrets`-style
  component (planned) backed by the `SecretsStage`, reading from mounted Secret
  volumes or a KMS.
- If a Secret value must live in a config file, mount it as a volume and point
  a source at it — the same symlink-swap handling applies. Never bake secrets
  into ConfigMaps or into the image.

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

- **Startup**: the file must exist when `AddSource` runs (`Init` time).
  `AddSource` fails fast, so a missing mount surfaces immediately instead of
  starting with defaults. If the mount may legitimately be late, register the
  source only after it is known to exist, or add an explicit readiness check
  before `fw.Run`.
- **File size**: each source file must be **1 MiB or smaller**
  (`MaxConfigFileBytes`). That is also the ConfigMap/Secret object size, so a
  normal mount already cannot exceed it. A bigger file (wrong `--<name>`
  path, a log dump bind-mounted over the config) fails the load; a reload
  keeps last-good. The cap is the file on disk, not YAML decode memory.
- **Rollouts**: `kubectl rollout restart` is still the safe way to apply a
  change if you want a clean process start; hot-reload is for changes that must
  not interrupt service.
- **Large ConfigMaps**: every file in the mount shares the watched directory.
  Events for unrelated keys are cheap (hash dedup) but will cause a re-read of
  the file; keep per-source directories small if you are extremely sensitive to
  I/O.
