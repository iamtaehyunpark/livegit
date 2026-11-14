# Live Git (`lg`) — Troubleshooting & Reference

## Failure → Diagnosis → Fix

| Symptom | Meaning | Fix |
|---|---|---|
| `not an lg project` | `cwd` isn't under a `.lg/` directory. | Run `lg scan` to find projects and `cd` into one. Do not run `lg init` unless creating a new project. |
| `not connected … run 'lg connect'` | No authenticated connection and `lg` cannot prompt. | Ask the user to run `lg connect` (Duo/2FA). Verify with `lg connect --check`. |
| `… interactive second authentication step (Duo/2FA)` | Project is in `native` SSH mode but the host requires Duo. | Run `lg config set source.ssh_mode system`, then ask the user to run `lg connect`. |
| `mount didn't come up — see …/lg.log` | FUSE couldn't start (macFUSE/libfuse missing?) or connection died mid-mount. | Read `.lg/lg.log`; report to the user if FUSE itself is absent. |
| `already mounted — an lg shell or lg mount is active` | The tree is already mounted. | That's fine—just use `<project>/<repo-name>/`. |
| `ENXIO` / `device not configured` touching the mount | Stale mount from a killed holder process. | Run `lg unmount`, then `lg mount`. |
| `lg run` command never returns | Remote program is waiting for interactive input. | Interrupt it; rerun non-interactively, or edit via the mount. |
| `flush barrier: timed out (continuing)` | A mount edit hadn't reached the server in 10s (offline or slow). | Check `lg status` (pending writes? connection?); rerun once the journal drains. |
| `lg status` shows pending writes that never drop | An entry in the write journal cannot be applied on the server (e.g., deleting a file owned by another user; log shows `flush parked`). | Inspect with `lg pending`; run `lg pending drop <path> -y` to abandon those entries (ask the user before `lg pending drop --all`, which discards unflushed local edits). |
| Log shows `lg: command not found` (remote) | Agent binary missing on the server. | Ask the user to run `lg connect` (auto-deploys/upgrades it), or re-run `lg init`. |
| `Permission denied` on a remote path | Server-side Unix permissions. | Report the permissions issue to the user. |

Primary diagnostic log: `<project>/.lg/lg.log` (run `lg config path` to locate `.lg/`).

## Editing Without a Mount (Fallback)

If you only need a single surgical file write without mounting, pass content via a base64 argument rather than piping stdin (the remote PTY does not signal EOF cleanly):

```sh
B64=$(printf '%s' "$NEW_CONTENT" | base64)
lg run -- bash -c "printf %s '$B64' | base64 -d > 'path/to/file'"
lg run -- cat path/to/file
```
