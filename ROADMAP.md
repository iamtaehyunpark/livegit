# ROADMAP — platform/architecture upgrade paths (execute per demand)

lg currently has exactly one user (the author, macOS Ghost + Linux Sources).
These are the pre-scoped upgrade paths for when that changes. None are needed
today; each has a trigger condition. The invariant that keeps every one of
them cheap: **all sync logic lives in `internal/fuse.Backend` + transport;
`node.go` is a thin filesystem skin.** Never let policy leak into the skin.

## 0. Preview-storm defense (DONE, v1.4.9)

Finder/QuickLook/indexers open every file they see; each open triggers a
download. Handled by reader-refcount abandonment: when the last consumer of an
in-flight fetch closes before it finishes, the fetch cancels after a 3s grace
(`fetchAbandonGrace`), unless nearly done (`fetchAbandonTail`). Real consumers
(cp/cat/drag-copy) hold their handle to EOF, so their fetches always complete.
Attribution logging ("download triggered" + pid/proc) stays for diagnosis.

## 1. Linux Ghost — trigger: a Linux user appears. Effort: days.

go-fuse is Linux-native; the Go code cross-compiles already. Work items:
- Verify mount/unmount/stale-recovery paths (`fusermount -u` vs `umount`).
- machine-id secret derivation (already handles `/etc/machine-id`), atime
  helpers (already build-tagged), shell hooks (zsh/bash fine).
- `make release` already builds linux binaries; test a Linux ghost end-to-end.
No storm problem there (thumbnailers are tame); no arch changes.

## 2. macOS onboarding without the kext — trigger: distributing to macOS
users who won't do the Apple Silicon Recovery-Mode kext dance. Effort: ~a day
of testing.

macFUSE 5+ has an FSKit-based backend (macOS 15+, Apple's user-space FS
framework): same FUSE API lg already speaks, no kernel extension, plain
System Settings approval. Test lg against it, document it as the recommended
install for macOS 15+. No code changes expected; watch macFUSE release notes
for FSKit backend maturity.

## 3. macOS FileProvider front-end — trigger: real distribution, or Apple
hard-deprecating the kext path, or wanting native cloud-badge UX (which kills
the preview-storm class entirely at the OS level). Effort: weeks.

Author holds an Apple Developer license, so signing/notarization is available.
Architecture: lg stays the CLI; a faceless signed helper app (LSUIElement,
shipped via `brew install --cask`) hosts an NSFileProviderReplicatedExtension
(Swift) that IPCs (unix socket) to the Go core:
- enumerator ⇄ Backend index; fetchContents ⇄ Materialize; change events ⇄
  journal/flush; mount surface moves to `~/Library/CloudStorage/…` (symlink
  from the project dir for the familiar layout).
- Release pipeline gains: Xcode build of the extension, codesign + notarize in
  `make publish`.
- Known regression to accept: standard hydration is all-or-nothing (our
  progressive streaming is better); NSFileProviderPartialContentFetching can
  mitigate on newer macOS but app support varies.
Performance is otherwise equal-or-better (hydrated files are native APFS —
no per-read interposition like FUSE).

## 4. Windows Ghost — trigger: a Windows user materializes. Effort: weeks
(touches four subsystems, not just the FS layer).

Route A (pragmatic, rclone-proven): WinFsp + cgofuse. Rewrite the FS skin
against cgofuse (concepts map 1:1 from go-fuse), which then covers WinFsp
(Windows) + macFUSE (mac) + libfuse (linux) with ONE front-end. Users install
WinFsp via its signed MSI — no security drama.
Route B (best UX, later): Cloud Files API (CFAPI) placeholders — Windows'
FileProvider, but callable from a normal exe, no signed bundle needed; scarce
Go bindings (cgo+COM), bigger lift.
Also needed regardless of route:
- PTY: `lg run` needs ConPTY (current pty lib is unix-only).
- Secrets: machine key from registry MachineGuid (ioreg//etc/machine-id today).
- Transport: Windows OpenSSH has no ControlMaster multiplexing → system-mode
  Duo-reuse doesn't translate; Windows ghosts live on native mode.
- Paths/exec: the usual filepath and shell-hook audit.

## 5. End-state shape (only if lg becomes a real product)

One sync core, per-platform thin front-ends — exactly how OneDrive/Dropbox
are built: FileProvider (macOS) + CFAPI (Windows) + FUSE (Linux), all
consuming `Backend`. Every step above is an addition, not a rewrite, as long
as the Backend/skin boundary stays clean.
