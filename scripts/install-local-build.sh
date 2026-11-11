#!/usr/bin/env bash
# install-local-build.sh - install a new local build of no-mistakes into the
# running service without failing a run that is in the middle of a step.
#
#   1. Write the hold file <NM_HOME>/hold (who, why, until). While it exists
#      `axi triage` and `axi run` stop and the daemon refuses every new run;
#      runs already in flight carry on.
#   2. Wait, with no time limit, until no run is in the middle of a step
#      (`<new binary> daemon busy` prints 0). A run parked at a gate does not
#      count: a clean stop keeps it and the next start resumes it at that gate.
#   3. Stop the service with `daemon stop --force` (its guard would refuse
#      while parked runs exist; they survive).
#   4. Back up the run database to <NM_HOME>/backups/install-<time>/.
#   5. Install the new binary over the one the service runs, keeping the
#      previous binary next to it as <target>.prev (the rollback copy).
#   6. Start the service and check the installed version and that the daemon
#      runs.
#   7. On any failure from step 3 on: put the previous binary and the backed-up
#      database back, start the service again, and exit non-zero.
#   8. Remove the hold file. If the rollback itself fails the hold stays, with
#      a reason saying the service needs a person.
#
# Usage: scripts/install-local-build.sh [--binary PATH] [--target PATH]
#          [--who TEXT] [--why TEXT] [--until TEXT] [--poll SECONDS]
#   --binary  the new build (default: bin/no-mistakes in this repository)
#   --target  the binary the service runs (default: `no-mistakes` on PATH,
#             symlinks resolved)
#   --who/--why/--until  the hold file's fields
#   --poll    seconds between checks while waiting (default 30)
# Exit: 0 installed; 1 nothing changed (bad input, a hold already exists, or
# stopped while waiting); 2 install failed and was rolled back; 3 rollback
# failed (the hold file is left in place).
set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
binary="$repo_root/bin/no-mistakes"
target=""
who="${USER:-$(id -un 2>/dev/null || echo unknown)}"
why=""
until_text="the install finishes (no time limit)"
poll=30

while [ $# -gt 0 ]; do
  case "$1" in
    --binary) binary="${2:-}"; shift ;;
    --target) target="${2:-}"; shift ;;
    --who) who="${2:-}"; shift ;;
    --why) why="${2:-}"; shift ;;
    --until) until_text="${2:-}"; shift ;;
    --poll) poll="${2:-}"; shift ;;
    -h|--help) awk 'NR>1 { if (!/^#/) exit; sub(/^# ?/, ""); print }' "$0"; exit 0 ;;
    *) echo "install-local-build: unknown argument $1" >&2; exit 1 ;;
  esac
  shift
done

say() { printf 'install-local-build: %s\n' "$*"; }
die() { say "$*" >&2; exit 1; }

resolve() {   # follow symlinks; plain path when readlink -f is unavailable
  readlink -f "$1" 2>/dev/null || printf '%s\n' "$1"
}

nm_home="${NM_HOME:-$HOME/.no-mistakes}"
hold="$nm_home/hold"
db="$nm_home/state.sqlite"

[ -x "$binary" ] || die "new build $binary is missing or not executable (run make build first)"
binary="$(resolve "$binary")"
if [ -z "$target" ]; then
  target="$(command -v no-mistakes 2>/dev/null)" || die "no installed no-mistakes on PATH; pass --target"
fi
[ -e "$target" ] || die "installed binary $target does not exist"
target="$(resolve "$target")"
[ "$binary" != "$target" ] || die "the new build and the installed binary are the same file"

new_version="$("$binary" --version 2>/dev/null)" || die "the new build does not run: $binary --version failed"
old_version="$("$target" --version 2>/dev/null)" || old_version="(unknown)"
why="${why:-install $new_version}"

if [ -e "$hold" ]; then
  say "a hold is already in place, so another install or hold is running; nothing changed:" >&2
  sed 's/^/  /' "$hold" >&2
  exit 1
fi

mkdir -p "$nm_home" || die "cannot create $nm_home"
printf 'who: %s\nwhy: %s\nuntil: %s\n' "$who" "$why" "$until_text" > "$hold" || die "cannot write $hold"
say "hold written: $hold (new runs are refused until it is removed)"

stopped=0        # set once the service has been stopped: failures then roll back
backup_dir=""
finished=0

remove_hold() { rm -f "$hold"; }

restore_database() {
  [ -n "$backup_dir" ] || return 0
  local f
  for f in state.sqlite-wal state.sqlite-shm; do rm -f "$nm_home/$f"; done
  for f in state.sqlite state.sqlite-wal state.sqlite-shm; do
    if [ -e "$backup_dir/$f" ]; then cp -p "$backup_dir/$f" "$nm_home/$f" || return 1; fi
  done
}

daemon_running() {   # $1 binary; a few tries, the service may take a moment
  local i
  for i in 1 2 3 4 5; do
    if "$1" daemon status 2>/dev/null | grep -q 'daemon running'; then return 0; fi
    sleep 1
  done
  return 1
}

rollback() {
  say "rolling back to $old_version"
  "$target" daemon stop --force >/dev/null 2>&1 || true
  local ok=1
  if [ -e "$target.prev" ]; then
    cp -p "$target.prev" "$target.rollback.tmp" && mv -f "$target.rollback.tmp" "$target" || ok=0
  fi
  restore_database || ok=0
  if [ "$ok" = 1 ] && "$target" daemon start >/dev/null 2>&1 && daemon_running "$target" \
     && [ "$("$target" --version 2>/dev/null)" = "$old_version" ]; then
    say "rolled back: $old_version is running again"
    remove_hold
    finished=1
    exit 2
  fi
  printf 'who: %s\nwhy: install of %s FAILED and the rollback failed too; the service needs a person (backup: %s)\nuntil: a person repairs the service\n' \
    "$who" "$new_version" "${backup_dir:-none}" > "$hold"
  say "ROLLBACK FAILED: the hold stays in place; database backup: ${backup_dir:-none}" >&2
  finished=1
  exit 3
}

fail() {   # a failure after the stop rolls back; before it, just release the hold
  say "$*" >&2
  if [ "$stopped" = 1 ]; then rollback; fi
  remove_hold
  finished=1
  exit 1
}

on_signal() {
  [ "$finished" = 1 ] && exit 1
  fail "interrupted"
}
trap on_signal INT TERM HUP

# 2. Wait until no run is in the middle of a step. No time limit.
last=""
while :; do
  if count="$("$binary" daemon busy 2>"$nm_home/.install-busy.err")"; then
    if [ "$count" = 0 ]; then break; fi
    note="$count run(s) in the middle of a step: $(tr -s ' ' < "$nm_home/.install-busy.err" | paste -sd ';' - | cut -c1-300)"
  else
    note="cannot read the run database yet: $(head -c 300 "$nm_home/.install-busy.err")"
  fi
  if [ "$note" != "$last" ]; then say "waiting: $note"; last="$note"; fi
  sleep "$poll"
done
rm -f "$nm_home/.install-busy.err"
say "no run is in the middle of a step"

# 3. Stop. Parked runs survive a clean stop and resume at their gate.
stopped=1
"$target" daemon stop --force || fail "daemon stop failed"

# 4. Back up the run database.
backup_dir="$nm_home/backups/install-$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$backup_dir" || fail "cannot create $backup_dir"
for f in state.sqlite state.sqlite-wal state.sqlite-shm; do
  if [ -e "$nm_home/$f" ]; then cp -p "$nm_home/$f" "$backup_dir/$f" || fail "database backup failed"; fi
done
[ -e "$db" ] && say "database backed up to $backup_dir"

# 5. Install, keeping the previous binary as the rollback copy.
cp -p "$target" "$target.prev" || fail "cannot keep the previous binary as $target.prev"
cp -p "$binary" "$target.new" && chmod 755 "$target.new" && mv -f "$target.new" "$target" \
  || fail "cannot install $binary over $target"

# 6. Start and check.
"$target" daemon start || fail "daemon start failed"
got="$("$target" --version 2>/dev/null)"
[ "$got" = "$new_version" ] || fail "installed version is '$got', expected '$new_version'"
daemon_running "$target" || fail "the daemon is not running after start"

# 8. Release the hold.
remove_hold
finished=1
say "installed $new_version (was $old_version); previous binary kept at $target.prev"
exit 0
