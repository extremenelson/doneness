# TODOS

## Product

### Decide the public name before v0.1.0

**What:** Pick the public name (module path, binary, repo) for the tool before the first tag.
**Why:** "newproj" describes the old scaffolder, not an enforced done-gate for agents. Every adopted repo's hook command names the binary, so renaming later breaks them.
**Pros:** Settles install paths and hook commands before they spread to other repos.
**Cons:** A product decision, not code; it needs a short naming session (human ~1h).
**Context:** /plan-eng-review D22 (2026-11-07). D15 writes the absolute binary path into local settings, so a rename after release forces `adopt --apply` again everywhere. See docs/designs/agent-ready-gate.md Open Question 6.
**Depends on / blocked by:** Nothing. Must land before tagging v0.1.0.
**Priority:** P1

## Gate

### Off-machine receipt verification (push notes + CI check)

**What:** Push `refs/notes/newproj-gate` and add a GitHub Action that accepts a PR only when a PASS receipt matches its tree, OS and arch.
**Why:** v1 receipts are local and advisory, so an agent with a shell could forge one. Verifying them in CI turns "done" into a guarantee that holds beyond one laptop (Approach B).
**Pros:** Closes the main caveat in the design's guarantee section; CI can skip work already proven locally.
**Cons:** Sizable (human ~1 week / CC ~3h); notes don't push by default, so this needs a choice between pushed notes and a commit trailer.
**Context:** /plan-eng-review D20. D18 already put GOOS/GOARCH in the cache key and receipt so a cross-platform PASS can't satisfy CI. D14 made notes PASS-only. See docs/designs/agent-ready-gate.md Open Question 1.
**Depends on / blocked by:** v1 gate; D18 receipt format.
**Priority:** P2

## Infrastructure

### Release pipeline (goreleaser + Homebrew tap)

**What:** Add .goreleaser.yaml and a tag-triggered release workflow for darwin/linux amd64/arm64 binaries, then a Homebrew tap.
**Why:** v1 installs only via `go install`, so non-Go users can't try it.
**Pros:** Prebuilt binaries widen the audience past Go developers.
**Cons:** Release plumbing to maintain (human ~2h / CC ~10min to start).
**Context:** /plan-eng-review D1 deferred this out of v1; D19 tracked it. v0.1.0 ships as a plain tag. Start from goreleaser's Go template once the README demo exists.
**Depends on / blocked by:** CI workflow (in v1) and the naming decision (P1 above).
**Priority:** P2

## Workflow

### Port wt-* and repo-adopt as a thin `newproj wt` layer

**What:** Port the bin/ helpers into an optional `newproj wt` subcommand: `wt done` calls `newproj gate` and refuses without a PASS receipt, and `--container` reuses repo-adopt's logic.
**Why:** One binary for strangers (premise #4), and `wt done` is the natural place to require a receipt.
**Pros:** The author's own loop gets a receipt-gated `wt done`; the bash helpers go away.
**Cons:** Work that strangers using gtr may never touch (human ~2 days / CC ~1h).
**Context:** /plan-eng-review D21. Premise #3 says don't compete with gtr, so keep it thin. bin/wt-new:13-20 already detects container vs `.wt` layouts. Gate exit codes from D8 make `wt done` a few lines.
**Depends on / blocked by:** v1 gate exit codes (D8).
**Priority:** P3

## Completed
