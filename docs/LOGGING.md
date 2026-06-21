# Logging and log rotation on MacVz nodes (#69)

A MacVz node runs two long-lived processes: the `macvz-kubelet` (the virtual
node) and, when privileged networking is enabled, the `macvz-netd` helper
daemon. This document defines where each one logs, how those logs are kept from
growing without bound on a long-running node, and the structured-logging
conventions that make the output diagnosable.

## Where logs live

| Process | Run mode | Destination |
| --- | --- | --- |
| `macvz-kubelet` | manual (foreground) | stderr — redirect to a file to retain it |
| `macvz-kubelet` | LaunchAgent (installed) | `<prefix>/var/log/macvz-kubelet.log` (stdout), `<prefix>/var/log/macvz-kubelet.err.log` (stderr) |
| `macvz-kubelet` | klog file logging | the path passed to `--log-file` |
| `macvz-netd` | launchd (installed) | `/var/log/macvz-netd.log` (stdout), `/var/log/macvz-netd.err.log` (stderr) |
| `macvz-netd` | manual (`sudo macvz-netd serve`) | stderr |

Both binaries log through [klog](https://github.com/kubernetes/klog), so they
share the same flags: `-v=<level>` for verbosity, `--logtostderr` (default
`true`), and the file options below.

The helper's log paths are not hard-coded into behavior — they are the
`StdoutPath`/`StderrPath` of its `LaunchdConfig` (`pkg/network/privhelper`), so a
custom install can redirect them and rotation follows automatically.

## Keeping logs bounded

### Helper daemon — newsyslog (automatic)

The helper runs under launchd `KeepAlive` and writes to its log files forever;
launchd never rotates them. So `macvz-netd install` also drops a
[`newsyslog`](https://www.unix.com/man-page/osx/5/newsyslog.conf/) config at
`/etc/newsyslog.d/macvz-netd.conf` (`DefaultNewsyslogPath`). macOS's periodic
`com.apple.newsyslog` job then rotates both helper logs:

- rotate once a file grows past **~5 MB** (`LogRotateSizeKB`, default 5000),
- keep **7** compressed (`bzip2`) archives (`LogRotateCount`),
- archives are size-driven (the `when` column is `*`).

`macvz-netd uninstall` removes the drop-in, so nothing is left behind. Rotation
is therefore on from the first run with no extra operator step. Because
`newsyslog` is invoked on a schedule, the size cap is a ceiling checked on that
cadence, not an exact trim point.

To tune it, set `LogRotateCount` / `LogRotateSizeKB` on the `LaunchdConfig`
before install, or edit the drop-in and let the next `newsyslog` run pick it up.
To opt out of MacVz managing rotation entirely, clear `NewsyslogPath` (the
installer then writes no drop-in and the logs grow until an external tool trims
them).

Inspect the installed config:

```sh
launchctl print system/com.github.chimerakang.macvz-netd   # running state
cat /etc/newsyslog.d/macvz-netd.conf                        # rotation policy
```

### Kubelet — LaunchAgent logs, klog file rotation, or newsyslog

When installed with `macvz-install.sh`, the kubelet LaunchAgent writes stdout
and stderr under `<prefix>/var/log/` (default `/usr/local/var/log/`) and the
installer pre-creates those files as the operator user. For manual runs, stderr
is captured by whatever supervises the process. Two ways to bound kubelet logs:

1. **klog built-in size cap.** Run with
   `--log-file=/usr/local/var/log/macvz-kubelet.log --log-file-max-size=<MB>`
   (klog starts a fresh file once the current one exceeds the size). Simple, no
   system config, but klog keeps only the current file.
2. **Redirect + newsyslog.** Redirect stderr to a file and add a `newsyslog`
   drop-in mirroring the helper's, when you want compressed archive retention:

   ```
   # /etc/newsyslog.d/macvz-kubelet.conf
   # logfilename                    owner:group  mode count size  when flags
   /usr/local/var/log/macvz-kubelet.log <user>:staff 644  7     5000  *    J
   ```

   Use the owner that runs the kubelet so `newsyslog` can rotate the file.

## Structured diagnostics

All logging uses klog's structured form (`klog.InfoS`/`klog.ErrorS`) with
key/value context rather than interpolated strings, so logs can be grepped and
parsed by operation context. The standard keys:

| Context | Keys | Emitted by |
| --- | --- | --- |
| Workload / Pod lifecycle | `pod` (`namespace/name`), `workloadID`, `podIP`, `phase` | provider create/delete/restart/recovery (`pkg/provider`) |
| Restart & recovery | `pod`, `workloadID`, `phase` (adoption), `RestartCount` | restart loop (#45), restart recovery (#66) |
| Node | `node` at registration, plus the node name baked into the pod-controller event source | `cmd/macvz-kubelet` |
| Helper operations | `name` (command), `args`, `exit`, plus refusal reasons (`op`, out-of-scope detail) | `pkg/network/privhelper/server.go` |
| Network changes | attach/detach with `pod`, `podIP`, `vmIP`; mesh/route operations | provider podnet, `pkg/network` |

Health-probe traffic is logged at `V(2)` so routine checks do not flood the log;
applied changes log at the default level. Sensitive material (keys, tokens,
secret values) is never logged — the same redaction policy the diagnostic bundle
enforces (see [DIAGNOSTIC_BUNDLE.md](DIAGNOSTIC_BUNDLE.md)).

When filing a bug, prefer `macvz-kubelet bundle`, which collects the live health
report, runtime/helper status, and (with `--log-file`) the kubelet/helper logs
into a redacted archive — see [DIAGNOSTIC_BUNDLE.md](DIAGNOSTIC_BUNDLE.md).

## Validation

- `pkg/network/privhelper`: `RenderNewsyslog` covers both log files with the
  configured/owner/mode/retention fields; install writes the drop-in and
  uninstall removes it; an empty `NewsyslogPath` writes none.
- Manual long-running smoke: leave a node up under load, confirm
  `/var/log/macvz-netd.log` rotates (archived `*.bz2` files appear, the live
  file stays under the cap) after `newsyslog` runs, and that the kubelet log
  stays bounded under the chosen mechanism.
