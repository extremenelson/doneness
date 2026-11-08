#!/usr/bin/env bash
# T1 probe (docs/designs/agent-ready-gate.md, decision D12).
#
# Question: when Claude Code runs in a *linked* git worktree, does it load the
# Stop hook from the MAIN checkout's .claude/settings.local.json? Design
# decision D2 assumes yes. If the answer is no, D12's contingency applies:
# `--shared` becomes adopt's default and D2 is reopened.
#
# Also records, for D7/A3: the hook process's working directory, the
# CLAUDE_PROJECT_DIR it sees, and the `cwd` field on its stdin.
# With --block, also checks F3: does a `systemMessage` show when the same
# JSON object blocks the stop?
#
# Usage:
#   scripts/t1-worktree-hook-probe.sh setup [--dir DIR] [--block]
#   scripts/t1-worktree-hook-probe.sh report [--dir DIR]
#   scripts/t1-worktree-hook-probe.sh clean [--dir DIR]
#
# Everything lives under DIR (default: ${TMPDIR:-/tmp}/newproj-t1-probe).
# Nothing outside DIR is written. Your global Claude settings are untouched.
set -euo pipefail

die() { echo "FAIL  $*" >&2; exit 1; }

cmd="${1:-}"; [ $# -gt 0 ] && shift
dir="${TMPDIR:-/tmp}"; dir="${dir%/}/newproj-t1-probe"
block=0
while [ $# -gt 0 ]; do
  case "$1" in
    --dir) [ $# -ge 2 ] || die "--dir needs a path"; dir="$2"; shift 2 ;;
    --block) block=1; shift ;;
    -h|--help) sed -n '2,22p' "$0"; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

repo="$dir/repo"
log="$dir/hook-log.jsonl"
marker="$dir/.t1-probe"

setup() {
  command -v jq >/dev/null || die "jq is required (brew install jq)"
  command -v claude >/dev/null || echo "WARN  claude not on PATH; you will need it to run the probe"
  [ -e "$dir" ] && die "$dir already exists; run '$0 clean${dir:+ --dir $dir}' first"

  mkdir -p "$dir/sessions"
  : > "$marker"
  : > "$log"
  dir="$(cd "$dir" && pwd -P)"; repo="$dir/repo"; log="$dir/hook-log.jsonl"

  # The hook lives outside every checkout, so only settings resolution decides
  # whether it runs. It must never fail the stop: errors are logged, exit 0.
  cat > "$dir/hook.sh" <<HOOK
#!/usr/bin/env bash
input="\$(cat)"
sid="\$(printf '%s' "\$input" | jq -r '.session_id // "unknown"' 2>/dev/null || echo unknown)"
jq -nc --arg ts "\$(date -u +%Y-%m-%dT%H:%M:%SZ)" --arg pwd "\$PWD" \\
  --arg project_dir "\${CLAUDE_PROJECT_DIR:-<unset>}" --arg raw "\$input" \\
  '{ts:\$ts, hook_pwd:\$pwd, claude_project_dir:\$project_dir,
    stdin:((\$raw|fromjson?) // {unparsed:\$raw})}' >> "$log" 2>/dev/null || true
if [ "$block" = 1 ] && [ ! -e "$dir/sessions/\$sid" ]; then
  : > "$dir/sessions/\$sid"
  printf '%s\n' '{"decision":"block","reason":"T1 probe: blocked once on purpose. Reply with the single word ok and stop again.","systemMessage":"T1 PROBE (F3): this systemMessage was sent WITH a block. Note whether you can see it."}'
else
  printf '%s\n' '{"systemMessage":"T1 PROBE: Stop hook fired here. Check hook-log.jsonl for the cwd it saw."}'
fi
exit 0
HOOK
  chmod 0755 "$dir/hook.sh"

  # Ordinary clone layout, as adopt leaves it (design premise 2).
  git init -q --initial-branch=main "$repo"
  git -C "$repo" -c user.name=probe -c user.email=probe@example.invalid \
    commit -q --allow-empty -m "probe: initial commit"

  # The hook goes ONLY in the main checkout's local settings (D2), with the
  # absolute hook path (D15). Exclude it so git status stays clean.
  mkdir -p "$repo/.claude"
  jq -n --arg cmd "$dir/hook.sh" \
    '{hooks:{Stop:[{hooks:[{type:"command",command:$cmd,timeout:60}]}]}}' \
    > "$repo/.claude/settings.local.json"
  echo ".claude/settings.local.json" >> "$(git -C "$repo" rev-parse --path-format=absolute --git-path info/exclude)"

  # Linked worktree #1: plain git, sibling .wt dir (design adopt step 5).
  git -C "$repo" worktree add -q -b probe-plain "$repo.wt/plain"

  # Linked worktree #2: git-worktree-runner, if installed. Its CLI is not
  # verified here; any failure is reported and skipped, not fatal.
  if git gtr --help >/dev/null 2>&1; then
    before="$(git -C "$repo" worktree list --porcelain | sed -n 's/^worktree //p')"
    if (cd "$repo" && git gtr new probe-gtr) >"$dir/gtr.out" 2>&1; then
      after="$(git -C "$repo" worktree list --porcelain | sed -n 's/^worktree //p')"
      gtr_path="$(comm -13 <(printf '%s\n' "$before" | sort) <(printf '%s\n' "$after" | sort) | head -1)"
      [ -n "$gtr_path" ] && echo "$gtr_path" > "$dir/gtr-path"
    fi
    [ -s "$dir/gtr-path" ] || echo "WARN  'git gtr new probe-gtr' did not create a worktree; see $dir/gtr.out"
  else
    echo "SKIP  git-worktree-runner ('git gtr') is not installed; probing plain git worktrees only"
  fi

  for d in "$repo.wt/plain" "$(cat "$dir/gtr-path" 2>/dev/null || true)"; do
    [ -n "$d" ] && [ -e "$d/.claude/settings.local.json" ] \
      && die "$d has its own settings.local.json; the probe would not isolate D2"
  done

  echo
  echo "OK    probe set up in $dir"
  echo
  echo "Run one short Claude session in each directory below. In each one:"
  echo "  1. cd into it and run: claude"
  echo "  2. accept the folder-trust prompt if asked"
  echo "  3. type: say hi"
  if [ "$block" = 1 ]; then
  echo "  4. it will be blocked once on purpose; note whether the 'T1 PROBE (F3)'"
  echo "     message is visible, reply 'ok', let it stop, then /exit"
  else
  echo "  4. let it answer and stop, then /exit"
  fi
  echo
  echo "  control (main checkout): $repo"
  echo "  plain git worktree:      $repo.wt/plain"
  [ -s "$dir/gtr-path" ] && echo "  gtr worktree:            $(cat "$dir/gtr-path")"
  echo
  echo "Then run: $0 report --dir $dir"
}

report() {
  [ -e "$marker" ] || die "$dir is not a probe directory (run setup first)"
  command -v jq >/dev/null || die "jq is required"
  dir="$(cd "$dir" && pwd -P)"; repo="$dir/repo"; log="$dir/hook-log.jsonl"

  echo "Hook events logged: $(wc -l < "$log" | tr -d ' ')"
  if [ -s "$log" ]; then
    echo
    jq -r '"  stdin.cwd=\(.stdin.cwd // "<none>")\n    hook_pwd=\(.hook_pwd)  CLAUDE_PROJECT_DIR=\(.claude_project_dir)  stop_hook_active=\(.stdin.stop_hook_active | tostring)"' "$log"
  fi
  echo

  # A location counts as "fired" when a hook event's stdin cwd is inside it.
  # The main checkout's own subdirectories never contain the worktrees here,
  # because the worktrees are siblings (repo.wt/), not children.
  fired() { jq -e --arg p "$1" 'select((.stdin.cwd // "") as $c | $c == $p or ($c | startswith($p + "/")))' "$log" >/dev/null 2>&1; }

  verdict=0
  check() {
    local label="$1" path="$2"
    if fired "$path"; then echo "PASS  $label: Stop hook fired ($path)"
    else echo "MISS  $label: no hook event with cwd under $path"; verdict=1; fi
  }
  check "control (main checkout)" "$repo"
  control_ok=$verdict
  verdict=0
  check "plain git worktree" "$repo.wt/plain"
  [ -s "$dir/gtr-path" ] && check "gtr worktree" "$(cat "$dir/gtr-path")"

  echo
  if [ "$control_ok" -ne 0 ]; then
    echo "INCONCLUSIVE: the hook did not fire even in the main checkout. Check that"
    echo "you ran a session there, accepted folder trust, and that $dir/hook.sh runs."
    exit 2
  elif [ "$verdict" -eq 0 ]; then
    echo "D2 HOLDS: linked worktrees use the main checkout's settings.local.json."
    echo "Also check above: for worktree sessions, does hook_pwd or CLAUDE_PROJECT_DIR"
    echo "point at the main checkout? If so, D7 (use stdin cwd) is load-bearing."
  else
    echo "D2 FAILS: a linked worktree ran without the main checkout's hook."
    echo "Apply D12's contingency: make --shared adopt's default and reopen D2."
    exit 1
  fi
}

clean() {
  [ -e "$dir" ] || { echo "OK    nothing to clean at $dir"; return; }
  [ -e "$marker" ] || die "$dir has no probe marker; refusing to delete it"
  # Remove worktree registrations before deleting the directories.
  if [ -d "$repo/.git" ]; then
    git -C "$repo" worktree list --porcelain | sed -n 's/^worktree //p' | tail -n +2 |
      while read -r wt; do git -C "$repo" worktree remove --force "$wt" 2>/dev/null || true; done
  fi
  rm -rf "$dir" "$repo.wt"
  echo "OK    removed $dir"
}

case "$cmd" in
  setup) setup ;;
  report) report ;;
  clean) clean ;;
  *) sed -n '2,22p' "$0"; exit 64 ;;
esac
