# Packaging: install, upgrade, rollback, uninstall

How to install MacVz on an Apple Silicon Mac as a managed system component, keep
it up to date, roll back a bad version, and remove it cleanly (issue #70). This
is the operator-facing companion to [RELEASE.md](RELEASE.md), which covers how
the signed/notarized artifacts are built.

## What gets installed

MacVz has two long-running pieces with different privilege needs:

| Component | Runs as | macOS service | Why |
| --- | --- | --- | --- |
| `macvz-netd` | root | LaunchDaemon | Owns `pf`/`route`/`wg`; the privileged network helper (#38/#40) |
| `macvz-kubelet` | the operator | per-user LaunchAgent | `apple/container` is a per-user service and **refuses to run as root** |

[`scripts/macvz-install.sh`](../scripts/macvz-install.sh) ties both behind one
lifecycle. The helper's own LaunchDaemon management lives in `macvz-netd`
(`install`/`uninstall`/`status`); the installer drives it and adds the kubelet
LaunchAgent, the shared config, and a **versioned layout** so upgrades are
reversible.

## Layout

```
<prefix>/libexec/macvz/
  versions/<version>/macvz-kubelet      # immutable per-version binaries
  versions/<version>/macvz-netd
  current -> versions/<version>         # symlink selecting the active version
  previous                              # text file: the version rollback returns to
<prefix>/bin/macvz-kubelet -> .../current/macvz-kubelet
<prefix>/var/log/macvz-kubelet*.log     # LaunchAgent stdout/stderr
/usr/local/sbin/macvz-netd              # stable helper binary managed by macvz-netd install
<etc>/config.yaml                       # shared config — survives upgrades
<etc>/pki/                              # serving TLS — survives upgrades
~<operator>/Library/LaunchAgents/com.github.chimerakang.macvz-kubelet.plist
/Library/LaunchDaemons/com.github.chimerakang.macvz-netd.plist
```

Defaults: `prefix` = `/usr/local`, `etc` = `/etc/macvz` (override with
`MACVZ_PREFIX` / `MACVZ_ETC`). Config and PKI live **outside** the versioned
tree, so upgrade and rollback never touch operator state.

## Install

From the release install bundle (`macvz_<version>_darwin_arm64.tar.gz` — both
signed binaries + this installer + a config template):

```sh
tar xzf macvz_<version>_darwin_arm64.tar.gz && cd macvz_<version>_darwin_arm64
sudo ./macvz-install.sh install --from . --config config.example.yaml
```

A fresh install:

1. stages the binaries under `versions/<version>` and points `current` at them;
2. seeds `<etc>/config.yaml` from `--config` **only if absent** (an existing
   config is always kept) — edit it (`nodeName`, `kubeconfigPath`, mesh/podnet)
   before the node joins, or generate one with
   `macvz-kubelet bootstrap … --out /etc/macvz/config.yaml`;
3. installs and starts the `macvz-netd` LaunchDaemon (with config-derived request
   policy when the config is present, #41);
4. installs and loads the kubelet LaunchAgent in the operator's GUI session.

Verify:

```sh
./macvz-install.sh status
kubectl get nodes        # the Mac should register and go Ready
```

## Upgrade

```sh
sudo ./macvz-install.sh upgrade --from <new-bundle-dir>
```

Stages the new version, flips `current` to it (recording the prior version in
`previous`), reinstalls the helper from the new binary, and reloads the kubelet
agent. **Config and PKI are untouched**, so node identity, keys, and settings
are preserved.

## Rollback

```sh
sudo ./macvz-install.sh rollback
```

Flips `current` back to the version in `previous` and restarts both services.
The previous version's binaries are still staged under `versions/`, so rollback
is instant and does not rebuild or re-download. Rollback is **reversible**:
running it again returns to the version you rolled back from (it swaps
`current` and `previous`).

> Rollback requires the previous version to still be staged. Uninstall removes
> all staged versions, so roll back *before* uninstalling if you may need it.

## Uninstall

```sh
sudo ./macvz-install.sh uninstall            # remove services + binaries, keep config/state
sudo ./macvz-install.sh uninstall --purge    # also delete <etc> config, PKI, and state
```

Uninstall boots out and removes the kubelet LaunchAgent, runs
`macvz-netd uninstall` (removes the LaunchDaemon, the helper binary, and its
socket), and deletes the versioned tree and the `bin` symlink. Without
`--purge`, `<etc>` is left in place so a later reinstall reuses the same node
identity; `--purge` removes it for a clean teardown.

Drain the node from Kubernetes first if it is part of a live cluster
(`macvz-kubelet remove …`, see [NODE_REMOVAL.md](NODE_REMOVAL.md)).

## Environment knobs

`macvz-install.sh` honours: `MACVZ_PREFIX`, `MACVZ_ETC`, `MACVZ_SOCKET`,
`MACVZ_USER` (operator the LaunchAgent runs as; defaults to `$SUDO_USER`),
`MACVZ_HOME_DIR` (operator home override for rehearsals or non-standard users),
`LAUNCHCTL`, `NETD`, and `MACVZ_DRY_RUN=1` (print mutating actions instead of
running them). Root is required for the default `/usr/local` prefix.

## Validating the lifecycle

The version-layout, upgrade, rollback, and config-preservation logic is covered
by a no-root rehearsal that runs everything in a throwaway temp prefix with
`launchctl` and `macvz-netd` stubbed:

```sh
make install-rehearsal      # ./scripts/macvz-install-rehearsal.sh
```

It asserts: fresh install seeds config and points `current`; upgrade preserves
an operator-edited config and records `previous`; rollback returns to the prior
version and is reversible; uninstall removes services/binaries but keeps config;
`--purge` removes config. A **live** install → upgrade → rollback → uninstall on
a real Mac (services actually started, node joins, then drains) is still
required for full acceptance.
