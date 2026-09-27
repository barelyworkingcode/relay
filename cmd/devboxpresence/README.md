# devboxpresence

Answers or cancels relay's LocalAuthentication presence dialog on the devbox,
so `devboxverify` (and eve's nightly) can drive owner-gated operations with no
one at the keyboard. It adds nothing to relay: no API, flag or environment
variable, and it imports nothing from `cmd/relay`. It finds the dialog the way
a person would, on screen, and acts on it through Accessibility and keyboard
events.

## CLI

```
devboxpresence answer --expect TEXT [--timeout 20s]
devboxpresence cancel --expect TEXT [--timeout 20s]
devboxpresence cancel --any            # sweep every open LocalAuthentication dialog
devboxpresence check [--password]
devboxpresence measure --relay PATH [--count 10] [--timeout 60s]
devboxpresence version                 # DIALOG\tready\tsource=<rev>
```

Stdout is exactly one line on every path, usage errors included:

```
DIALOG\t<answered|cancelled|swept|ready|refused|none>\t<detail>
```

Progress goes to stderr. Right after it snapshots the windows already open,
`answer` and `cancel` write `devboxpresence: ready` to stderr. A caller that
starts the helper before its trigger should wait for that line before
triggering; a dialog that lands before the snapshot is refused as already open.

| exit | meaning |
|---|---|
| 0 | did what was asked and the dialog closed (`--any`: none left; `check`: ready) |
| 1 | no new dialog appeared within `--timeout`; nothing typed |
| 2 | usage error |
| 3 | refused to act; typed and pressed nothing. The detail names the reason |
| 4 | the password file is unusable |
| 5 | acted, but the dialog was still open 5 s later |

## Responsibility, signing and the grant

Under a launchd job, macOS judges Accessibility trust by the job's responsible
process, which is the job's leader (eve's nightly is led by `node`), not by
this binary. So at startup the helper re-spawns itself with the same argv and
environment through `posix_spawn` with
`responsibility_spawnattrs_setdisclaim` set. The child is its own responsible
process and is judged by its own grant. The parent forwards SIGTERM and SIGINT
to the child, shares its stdin, stdout and stderr, and exits with its code
(128 + the signal if it was killed). The child carries
`DEVBOXPRESENCE_DISCLAIMED=1` so it does not re-spawn again; setting it by hand
only skips the disclaim. If the re-spawn fails, the helper prints a `refused`
line and exits 3.

Disclaiming means an ad-hoc build has no grant of its own: run from an SSH
shell, whose trust comes from sshd's side, `check` reports `ax_trusted=no`.
The helper that runs unattended is the installed one, signed with a Developer
ID identity, so its grant survives rebuilds. devboxWorld's `bootstrap.sh`
builds it, signs it, installs it at
`~/.local/share/devboxverify/bin/devboxpresence` and grants it Accessibility
once. Relay only owns the source.

`version` reports the relay source the binary was built from. The convention
is the last commit that touched this directory, so unrelated relay commits do
not make an installed helper stale:

```bash
rev=$(git -C <relay checkout> log -1 --format=%H -- cmd/devboxpresence)
go build -ldflags "-X main.sourceRev=$rev" -o <out> ./cmd/devboxpresence
```

A build without the flag reports `source=unknown`. devboxverify's `helpers`
preflight compares this rev with its own checkout's, checks the Developer ID
signature, and refuses to run a missing, stale or unsigned helper.

## What it checks before acting

- **Session.** The user owns the console session (IORegistry `IOConsoleUsers`),
  the screen is unlocked, the window list is readable, and the process is
  trusted for Accessibility and may post events. It only preflights these;
  it never asks macOS for a permission, so it never raises a TCC dialog.
- **Owner.** A candidate is an on-screen window whose process is `coreautha`.
  It counts only if that process satisfies
  `anchor apple and identifier "com.apple.LocalAuthentication.UIAgent"`.
- **New.** `answer` and `cancel --expect` refuse if any agent window from the
  start snapshot is still open, or if more than one new one appears. Only
  `cancel --any` touches dialogs that were already open.
- **Text.** The window's text is read through AX, skipping the password field,
  and re-read until two reads agree. It must contain `--expect`, which names
  the request (the run nonce, or the id acted on), so the helper never
  approves someone else's request. Two callers carry no nonce: the P4
  renewal expects relay's whole reason for that mint, closing period
  included, and the eve enrolment expects relay's fixed reason. Unreadable
  text refuses `answer`; `cancel --expect` still cancels, since cancelling
  approves nothing.
- **Just before typing.** `answer` counts the agent's windows again and
  re-reads the dialog's text after focusing the password field. Anything but
  exactly this one window, still containing `--expect`, exits 3 with nothing
  typed.
- **Focus.** `answer` types only after the agent is the focused application
  and its secure field holds focus.

## The password

`answer`, `check --password` and `measure` read `$DEVBOX_ADMIN_PASSWORD_FILE`
(default `~/.claude/devbox-admin-password`). It must be a regular file (a
symlink is refused), with no group or other permission bits, and non-empty.
One trailing newline is stripped. The bytes are read into an exact-size
buffer, converted to UTF-16 in a preallocated buffer, posted as Unicode key
events at the HID tap in chunks of 20, then zeroed. The password never
appears in argv, stdout, stderr or a detail, which is why this helper posts
keystrokes itself instead of calling `computer type`.

## Measuring it (needs the screen)

These steps raise real dialogs. Run them only while holding the screen.

`measure` drives relay's own prompt: it runs
`relay mcp register --id devboxpresence-measure-<nonce>-<mode>-<i> --command /usr/bin/true`,
answers `--count` of them, then cancels `--count` more. `/usr/bin/true` never
speaks MCP, so an answered prompt fails discovery and nothing is registered;
there is nothing to clean up. D is timed from helper start to the dialog
closing. Each iteration logs to stderr, including the CLI's exit and whether it
said `presence was refused` (expected for cancels; answers fail discovery
instead). The first dialog text read through AX is printed once. On any
non-zero exit it sweeps and stops.

relay refuses to prompt for a caller without graphic access, so `measure`
cannot run from an SSH shell. Run it as a one-shot LaunchAgent in the GUI
domain, which also answers whether the helper is Accessibility-trusted there.

```bash
cd <relay checkout>
R=$(mktemp -d)
go build -o "$R/devboxpresence" ./cmd/devboxpresence

oneshot() {  # oneshot LABEL -- argv...
  local label=$1; shift 2
  local plist="$R/$label.plist" args=""
  for a in "$@"; do args="$args<string>$a</string>"; done
  cat >"$plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>$label</string>
  <key>ProgramArguments</key><array>$args</array>
  <key>WorkingDirectory</key><string>$PWD</string>
  <key>RunAtLoad</key><true/>
  <key>StandardOutPath</key><string>$R/$label.out</string>
  <key>StandardErrorPath</key><string>$R/$label.err</string>
</dict></plist>
EOF
  launchctl bootstrap "gui/$(id -u)" "$plist"
  until grep -q '^DIALOG' "$R/$label.out" 2>/dev/null; do sleep 1; done
  launchctl bootout "gui/$(id -u)/$label"
  cat "$R/$label.out"
}

# (3) Accessibility trust and console session under a one-shot LaunchAgent.
oneshot dev.devbox.presence-check -- "$R/devboxpresence" check --password

# (1) D over 10 answers and 10 cancels, and (2) whether AX reads coreautha's text.
oneshot dev.devbox.presence-measure -- "$R/devboxpresence" measure \
  --relay /Applications/Relay.app/Contents/MacOS/relay --count 10
cat "$R/dev.devbox.presence-measure.err"   # per-iteration D and the AX text

[ -n "$(ls -A "$R")" ] && rm -rf -- "${R:?}"
```

Reading the results:

- `check` prints `DIALOG ready console=yes … ax_trusted=yes post_events=yes`
  when the helper can act under launchd. Because the helper disclaims
  responsibility, TCC judges the helper binary itself, and an ad-hoc-signed
  Go binary's identity changes on every rebuild. Point `oneshot` at the
  installed, Developer ID signed helper rather than `$R/devboxpresence` to
  measure trust; `ax_trusted=no` for it means the bootstrap grant is missing.
- `measure` ends with
  `DIALOG ready answer n=10 mean=…s max=…s; cancel n=10 mean=…s max=…s; ax_text=readable`.
  `ax_text=unknown` together with exit 3 "unreadable" means AX cannot read
  coreautha's text. Escalate it.
- (4) A full `devboxverify` run as a one-shot LaunchAgent uses the same
  recipe: build `./cmd/devboxverify` into `$R` and pass its absolute path and
  flags, `--phase screen` among them if wanted, to `oneshot`. launchd's
  `PATH` has no `go`, so build first rather than using `go run`.
