# newproj

Make a repository safe for parallel Claude Code agents: one command gives it
an **enforced definition of done**. A Claude Code Stop hook runs the repo's own
check before an agent may finish, the check is only allowed to verify (never
rewrite files), checks share a machine-wide concurrency cap, and every pass
leaves a receipt tied to the exact tree that was tested.

```sh
go install github.com/extremenelson/newproj@latest

cd your-repo
newproj adopt            # dry run: shows the detected check and every file it would change
newproj adopt --apply    # write them, run the gate once: READY / NOT READY
```

## Commands

| Command | What it does |
|---|---|
| `newproj adopt [--apply]` | Retrofit the current repository (see below). Dry run by default. |
| `newproj gate` | Run the check from `.newproj.json` and record a receipt when it passes. |
| `newproj gate --status [<rev>]` | Is the working tree (or `<rev>`) proven? Never runs the check. |
| `newproj gate --hook` | Claude Code Stop-hook mode (installed by `adopt` and `new`; you don't run it by hand). |
| `newproj new [flags] <name>` | Scaffold a new Go project with the gate already wired in. |

`newproj [flags] <name>` still works as a deprecated alias for `newproj new`.

### Exit status

| Command | 0 | 1 | 2 | 3 | 4 | 130 |
|---|---|---|---|---|---|---|
| `gate` | pass (or cached pass) | check failed | config error, incl. command not found | the check changed the tree | timed out | interrupted |
| `gate --status` | proven | not proven | error | | | |
| `adopt` | plan shown, or READY | NOT READY (files written, gate red) | error | | | |

"Command not found" is detected from the shell's exit 127 on a best-effort
basis: a tool missing inside `make` or later in an `&&` chain shows up as an
ordinary failure.

## `newproj adopt`

Adopt detects the repository's own check command, in this order:

1. **Makefile / justfile:** an aggregate target (`check`, `ci`, `verify`), else
   the existing `lint`, `typecheck` and `test` targets joined with `&&`.
2. **package.json:** the same rule for scripts, run with the package manager
   its lockfile names (pnpm, yarn, bun, else npm).
3. **Toolchain defaults:** `cargo clippy -- -D warnings && cargo test` for
   Cargo, `go vet ./... && go test ./...` for Go.

Targets and scripts that rewrite files (`--fix`, `--write`, `-w`, a formatter
or `tidy` without a check flag) are excluded, including through prerequisites,
and the plan says why. Pass `--cmd "<command>"` to choose the command yourself;
a re-run keeps the command already in `.newproj.json`.

With `--apply` it writes:

- **`.newproj.json`**: `{"gate": {"command": "..."}}`. Commit it, so new
  worktrees inherit the gate.
- **The Stop hook**: by default in your personal
  `.claude/settings.local.json` in the main checkout (Claude Code reads that
  file for every worktree), pinned to this binary's absolute path, and added to
  `.git/info/exclude` if git doesn't already ignore it. With `--shared` it goes
  in the committed `.claude/settings.json` instead, as a minimal edit that
  keeps the file's formatting, looking `newproj` up on `PATH`. If the binary is
  missing, the hook lets the stop through with a visible warning rather than
  trapping the agent.
- **A rules block** in `CLAUDE.md` (else `AGENTS.md`, else a new `CLAUDE.md`)
  between `<!-- newproj:begin ... -->` and `<!-- newproj:end -->` markers. A
  re-run replaces it.

It never moves the repository, commits, pushes or touches CI files. Running the
gate records a receipt in `refs/notes/newproj-gate` (git notes are not pushed
by default) and keeps state under `$XDG_STATE_HOME/newproj`
(`~/.local/state/newproj` by default).

## How the gate works

1. **Tree hash.** The gate hashes the work tree as it is on disk (tracked,
   modified and untracked-but-not-ignored files) through a temporary copy of
   the index; the real index is never touched.
2. **Cache.** If a receipt for that tree matches the command, the config, the
   newproj version and the OS/arch, it is a cached pass: nothing reruns.
3. **Slot.** Checks share a machine-wide cap across every repository and
   worktree: `$NEWPROJ_SLOTS`, else `{"slots": N}` in
   `~/.config/newproj/config.json`, else a quarter of the CPUs.
4. **Run.** `sh -c <command>` at the repository's top level, in its own process
   group, with `CI=1` (unless already set) and an empty stdin, so tools take
   their non-interactive, non-writing path. On a timeout or Ctrl-C the whole
   process group is stopped; leftover background processes are killed too.
5. **Guard.** If the tree changed while the check ran, the gate fails
   (exit 3): a gate must only verify.
6. **Receipt.** A pass is recorded as a git note on the tree. Failures are kept
   locally, so `--status` can say `unproven (last attempt RED: failed)`.

### In Claude Code

The Stop hook runs the gate on every stop. While it is red, the agent is sent
back with the failure, the files the check changed, and the last 60 lines of
output. After 3 red attempts in a session the stop is allowed but reported as
**NOT DONE**, so a gate that cannot go green never traps the agent.

### What the guarantee covers

- **Claude Code only.** The Stop hook is a Claude Code feature. Other agents
  get the rules block, which is advisory.
- **Mistakes, not adversaries.** The gate stops an agent from finishing early
  or over a red check. An agent with a shell could still edit `.newproj.json`,
  remove the hook or write a fake note; `gate --status` makes that visible
  (config drift since the default branch, a missing hook) rather than
  impossible. Receipts are local and advisory.

### What the cache does not see

A receipt is keyed to the tree, the command and config, the newproj version and
the OS/arch. It does not cover environment variables, tool versions, ignored
files (`.env`, `node_modules`, build output) or the working trees of
submodules (recorded only as their commit). If any of those matter, run
`newproj gate --no-cache`. Rebuilding newproj changes its version, so receipts
from an earlier build stop counting.

Every gate run stores the hashed files as loose git objects; a repository with
large untracked, unignored files grows `.git/objects` until git's own garbage
collection runs.

## `newproj new`

```sh
newproj new -prefix github.com/you demo     # container layout: demo/.bare + demo/main
newproj new -flat demo                      # an ordinary single checkout
```

The module path prefix comes from `-prefix`, else `$NEWPROJ_PREFIX`, else
`github.com/<git config github.user>`. The project gets a Makefile whose
`check` only verifies (`make fix` rewrites formatting and `go.mod`),
golangci-lint config, CI, agent rules, `.newproj.json` and the Stop hook in
its committed `.claude/settings.json`. The container layout needs git 2.42+.

## Requirements

macOS or Linux, git 2.42+ for the container layout, and `sh`. Claude Code for
the Stop hook.

## Helper scripts

`bin/` holds the author's worktree helpers (`wt-new`, `wt-done`,
`repo-adopt`, ...). `make install-scripts` copies them to `~/bin`. They are not
needed to use the gate.

## Development

```sh
make check   # read-only: format diff, go.mod diff, lint, tests with -race
make fix     # rewrite formatting and go.mod/go.sum
```

Design and review notes: [`docs/designs/agent-ready-gate.md`](docs/designs/agent-ready-gate.md).
