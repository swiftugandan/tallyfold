# Tallyfold

Tallyfold gives coding agents memory that lasts across sessions, projects and
machines. The memory lives in a private git repository you own, as plain Markdown
you can read and edit. Tell your agent something once, and the next session, in any
project on any machine, starts out knowing it.

Website: <https://swiftugandan.github.io/tallyfold/>

Tallyfold is an [Agent Plugins 1.0](https://agent-plugins.org/specification) package
with a Claude Code extension. Memory files follow the
[Agent Memory Repo](https://github.com/AgentMemoryRepo/agentmemoryrepo) format. This
release keeps personal memory; team and swarm memory are designed and have seams in
the code (see [docs/design.md](docs/design.md)).

## Install in Claude Code

Tallyfold runs on macOS and Linux and needs `git` and `curl`.

```sh
claude plugin marketplace add swiftugandan/tallyfold
claude plugin install tallyfold@tallyfold
```

Create an empty private repository for your memory, then ask your agent to use it:

```sh
gh repo create agent-memory --private
```

> Set up Tallyfold with git@github.com:you/agent-memory.git

Tallyfold reaches the repository with your machine's own git credentials (SSH agent
or credential helper). To try it without a remote, ask the agent to set up Tallyfold
locally; asking again later with a URL gives that local repo a remote. An existing
Agent Memory Repo repository (one with a `MEMORY.md`) works too.

## Other agent clients

The [`plugin/`](plugin) folder is a conformant Agent Plugins 1.0 package with one
skill and one stdio MCP server, so clients such as VS Code, Cursor, GitHub Copilot
and Codex can install it from this repository. Only Claude Code has been tested.
The specification has no session-start hook, so in other clients memory loads when
the agent first calls `memory_context`, which the skill and the server's
instructions tell it to do.

## How it works

At the start of each Claude Code session a hook pulls the latest memory and prints
`MEMORY.md` into the session. When the agent learns something worth keeping, it calls
`memory_write`. Tallyfold adds one line to a Markdown file, stamps the date and the
client that wrote it, commits, and pushes:

```markdown
# Memory

- Prefers a table when comparing options, with the recommended one first [added: 2026-10-07; by: claude-code]

## Index

- [[people]]
- [[projects/payments]]
```

`MEMORY.md` holds what every session needs and stays under 4,000 characters; other
facts go in topic files that the index links. The agent replaces or removes an entry
when a fact changes, and git keeps every earlier version.

| MCP tool | What it does |
|---|---|
| `memory_init` | set up memory from an empty or existing repo, or locally; give a local repo a remote later |
| `memory_context` | load `MEMORY.md` with its index |
| `memory_search` | find lines that contain all the given words |
| `memory_read` | open a file or follow a `[[link]]` |
| `memory_write` | add, replace or remove one entry; commit and push |
| `memory_sync` | pull other sessions' memory and push waiting changes; resolve a conflict with `prefer` |
| `memory_status` | show the remote, the local copy and unpushed changes |

The same binary is a CLI (`tallyfold status`, `tallyfold search …`,
`tallyfold lint`, …). Run `tallyfold help` for the list.

Two sessions that write different facts at the same time both land: Tallyfold
rebases onto the newer remote and pushes again. If they change the same line, the
later write is undone and the agent is shown both versions so it can write one
merged entry. If the remote is unreachable, writes are committed locally and pushed
at the next sync.

## Privacy and safety

- Memory is stored in your repository and in a clone in the client's plugin data
  directory (for Claude Code, `~/.claude/plugins/data/tallyfold-tallyfold/`). There
  is no Tallyfold service.
- Writes that look like credentials (private keys, cloud and API tokens, password
  assignments) are refused before they are committed.
- Tallyfold turns off commit signing for its own commits, because a signing prompt
  would hang a background server.
- The launcher downloads the release binary on first use and runs it only if its
  SHA-256 matches the checksum committed in the plugin. Release binaries are built
  reproducibly and carry GitHub build provenance attestations.

## Repository layout

```text
.claude-plugin/marketplace.json  Claude Code marketplace entry (manifest, MCP config, hook)
plugin/                          the Agent Plugins package
  plugin.json, mcp.json          portable manifest and MCP server
  skills/tallyfold/SKILL.md      when and how to read and write memory
  bin/tallyfold, bin/release     launcher, version and checksums
  com.anthropic.claude-code/     Claude Code's MCP config
cmd/tallyfold/                   CLI, MCP server and hook entry point
internal/                        entry format, git, mounts, publish policies, memory, MCP tools
site/                            the website (GitHub Pages)
design/og.html                   source of the social preview image
docs/design.md                   design, decisions and seams for team and swarm memory
```

## Development

Go comes from [mise](https://mise.jdx.dev) (`mise.toml`).

```sh
make test   # unit and integration tests; the integration tests drive real git
make lint   # gofmt, go vet, shellcheck
make dev    # build for this machine into plugin/bin/dev/, which the launcher prefers
```

To try a change in Claude Code without installing it, build with `make dev` and run
a session with the repository as a local marketplace:

```sh
claude --settings '{"extraKnownMarketplaces":{"tallyfold":{"source":{"source":"directory","path":"'"$PWD"'"}}},"enabledPlugins":{"tallyfold@tallyfold":true}}'
```

That registers the marketplace in `~/.claude/plugins/`; remove it afterwards with
`claude plugin marketplace remove tallyfold`.

`internal/pluginpkg` checks the package against the Agent Plugins specification and
keeps the Claude Code entry, manifest version and release file in step.

## Releasing

```sh
make bump NEW=0.2.0   # sets the version everywhere and records checksums in plugin/bin/release
git commit -am "Release 0.2.0" && git push
git tag v0.2.0 && git push origin v0.2.0
```

CI rebuilds the binaries on every push and fails if they differ from the committed
checksums. The release workflow rebuilds them again for the tag, attests them, and
publishes them to the GitHub release that the launcher downloads from.

## License

[MIT](LICENSE)
