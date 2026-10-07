# Tallyfold design

This document explains how Tallyfold is built, why, and where team and swarm memory
attach. Read it before changing the package layout, the write path, or the release
process.

## Goals

Tallyfold gives agents memory that outlives a session and follows the user across
projects and machines. Four constraints shape the design:

- The user owns the memory. It is a git repository on their account, in plain
  Markdown, with full history. Tallyfold runs no service.
- Memory must load without anyone asking, or it does not get used.
- Concurrent sessions must never lose each other's writes.
- One package should work in every agent client that supports the Agent Plugins
  specification, with client-specific behaviour kept in client extensions.

## Scopes

| Scope | Repo | How writes land | Status |
|---|---|---|---|
| Personal | the user's private repo | direct push to `main` | built |
| Team | a repo the team can read | proposal branch, checks, curator merge | planned |
| Swarm | a branch in the repo that owns the task | direct push, append-only logs | planned |
| Guest | someone else's personal repo | read-only | planned |

Personal memory takes direct pushes because one person owns it and git history makes
every write reversible. A wrong team entry would mislead every teammate's agent, so
team writes will go through review. Swarm agents need each other's findings within
minutes, so swarm writes will push straight to a branch that stays out of `main`.

## Code structure

```text
cmd/tallyfold     entry point: CLI, `mcp` (stdio MCP server), `hook session-start`
internal/entry    one entry = one Markdown bullet with [key: value; …] metadata
internal/gitx     runs git without prompts; classifies rejected pushes, conflicts, offline
internal/mount    mounts (clones) in the plugin data dir, their roles, the mount list, locks
internal/publish  policies that share a mount's commits; DirectPush for personal memory
internal/memory   init, context, search, read, write, lint, status on top of mounts
internal/mcpserver  the MCP tools and server instructions
internal/pluginpkg  conformance tests for the plugin package
```

`memory` is the module agents and people use, and it knows nothing about how a
mount's changes are shared. `publish` owns that, behind one interface:

```go
type Policy interface {
    Sync(ctx, m *mount.Mount, prefer gitx.Prefer) (Outcome, error)
    Publish(ctx, m *mount.Mount, made string) (Outcome, error)
}
```

## Seams for team and swarm memory

The places where team and swarm memory attach already exist and are exercised:

- **Mount roles.** `mount.Role` names `personal`, `team`, `swarm` and `guest`.
  `Role.Supported()` reports `personal` only today.
- **The mount list.** The personal repo may hold `.tallyfold/mounts.toml` with
  `[[mount]]` entries (`name`, `url`, `role`). `Layout.Load` reads it and reports each
  entry it cannot mount as a note in the startup context and in `memory_status`, so
  a list written for a later version never breaks an older one.
- **Publish policies.** `publish.For(role)` returns `DirectPush` for personal memory
  and `ErrUnsupported` for the rest. Team memory adds a policy whose `Publish` pushes
  a proposal branch and opens a pull request, and whose `Sync` fast-forwards `main`.
  Swarm memory adds one that pushes to the swarm branch, with `merge=union` on the
  shared logs.
- **The write scope.** `memory_write` takes `scope`, which defaults to `personal`.
  Other scopes return a clear "not supported in this version" error, so clients
  written against a later version degrade cleanly.
- **Context and search** already loop over every mount and label each section with
  its mount name, and `memory_read` accepts `mount:path` targets.

Adding team memory therefore means: mark `Team` supported, clone listed team mounts
in `Layout.Load`, add the proposal policy, and add the team checks to CI in the team
repo. Nothing in `memory` or the MCP tools needs to change shape.

## Write path

`Store.Write` takes the mount lock, then:

1. Syncs first (fetch, rebase local commits, push), so the change applies to the
   newest copy. A conflict here stops the write and is reported.
2. Plans the change in memory: parses the target file, finds the entry for
   `replace` and `remove` (exactly one match is required), stamps `added` and `by`,
   refuses credentials, and refuses to grow `MEMORY.md` past 4,000 characters. A new
   topic file is created with a heading and linked from the `MEMORY.md` index.
3. Writes the files and commits exactly those paths.
4. Publishes. `DirectPush` pushes; on a rejection it fetches and rebases, up to three
   times. If the rebase conflicts, it aborts, drops only the commit it just made
   (checked by hash), and returns a `ConflictError` with both versions. If the
   remote is unreachable, the commit stays local and the outcome is `pending`; the
   next sync pushes it.

Writes are serialised per mount on one machine with `flock` on
`mounts/<name>.lock`, so two sessions on the same machine cannot interleave git
commands in one clone.

`memory_sync` with `prefer: remote` or `prefer: local` resolves a conflict between
earlier local commits and the remote by rebasing with `-X ours` or `-X theirs`.

## Startup context

Measured on Claude Code 2.1.292 with a probe plugin:

- MCP server instructions reach the model but are cut at 2,048 characters. Tallyfold's
  instructions carry only the rules; a test keeps them under the limit.
- `SessionStart` hook output is shown inline up to about 10,000 characters (9,500
  stayed inline; 24,000 became a file reference with a short preview). The hook syncs
  for up to 8 seconds, then prints each mount's `MEMORY.md` within a 9,000-character
  budget. It never fails the session.

The Agent Plugins specification has no hooks, so other clients load memory when the
agent calls `memory_context`, prompted by the skill and the server instructions.

## Packaging

The `plugin/` directory is a conformant Agent Plugins 1.0 package: `plugin.json`,
`mcp.json`, `skills/`, `bin/`, `LICENSE`, and the `com.anthropic.claude-code/`
extension directory. `internal/pluginpkg` enforces the closed manifest, the stdio
server rules, skill names, and that nothing client-specific sits outside a namespace
directory.

Claude Code 2.1.292 does not implement the specification. Tested behaviour, and the
response to each point:

| Spec part | Claude Code | Response |
|---|---|---|
| root `plugin.json` | ignored | its manifest lives in the marketplace entry, with `strict: false` |
| `skills/` | loaded | shared |
| `mcp.json` | not read; `${PLUGIN_ROOT}` is treated as an unset variable | `com.anthropic.claude-code/mcp.json` in Claude Code's format |
| `PLUGIN_ROOT`, `PLUGIN_DATA` | not set; it sets `CLAUDE_PLUGIN_*` | that file maps them into the server's env; the launcher reads either |
| hooks in a marketplace entry | path form rejected | the hook is inline in the entry |

The marketplace entry lives in `.claude-plugin/marketplace.json` at the repository
root, outside the package, so the package stays conformant. VS Code reads the same
marketplace format.

## Binaries and the launcher

The spec requires `command` to be one executable token, and the package is installed
from git, where committing binaries for four platforms on every release would bloat
history. So `plugin/bin/tallyfold` is a POSIX launcher. On first use it downloads
`tallyfold_<version>_<os>_<arch>` from the GitHub release named in `plugin/bin/release`,
checks it against the SHA-256 recorded there, and stores it in the plugin data
directory, which clients keep across updates. Older binaries are removed after a new
one is installed. A binary in `plugin/bin/dev/` (from `make dev`, git-ignored) takes
precedence for development.

The checksums are committed before the release exists, which works because builds
are reproducible: `CGO_ENABLED=0`, `-trimpath`, `-buildvcs=false`, an empty build ID,
pinned `GOAMD64`/`GOARM64`, and the Go toolchain pinned in `mise.toml`. CI runs
`make verify` on every push and the release workflow runs it again before
publishing, so a published binary always matches the checksum in the plugin.

## Limitations

- macOS and Linux only; the launcher is a POSIX shell script.
- Tallyfold uses the machine's git credentials. The spec says a plugin should not
  depend on ambient environment such as `HOME` or `SSH_AUTH_SOCK`; a later version
  can replace this with a GitHub App device flow and a keychain token.
- Each client has its own plugin data directory, so two clients on one machine keep
  two clones. Git keeps them in step, but a write in one is visible to the other only
  after its next sync.
- Uncommitted hand edits inside the clone block rebases until they are committed or
  discarded; edit memory through GitHub or your own clone instead.
- Commit signing is turned off for Tallyfold's commits.
