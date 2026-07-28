#!/bin/bash
# pg-guard -- docker/get-status.sh -- dumps GET /status from both nodes.
# Default: one full JSON dump each, via jq. -w: watch mode -- instead
# clears the screen and prints a two-column table (t-0 vs. t-1 side by
# side) of just the fields worth eyeballing while something's actually
# happening: role, reachability, replication lag, every "something's in
# progress" flag (including backup_in_progress), plus two computed rows per
# node: time_since_change (how long since any of that node's own tracked
# fields last differed from the previous cycle -- each node tracked
# independently, since they can settle at different times) and last_backup
# (how long since that node's own last backup *attempt* -- "never" if none
# this run, "FAILED <time>" if the last attempt didn't succeed, with the
# actual error text printed below the table in that case -- deliberately
# the attempt fields, not the success-only last_backup_timestamp_seconds,
# so a currently-broken schedule is visible immediately instead of just
# looking like "hasn't run in a while"; computed directly from /status, not
# from the change-detection mechanism above, since a backup completing
# between polls doesn't necessarily change any of the other tracked
# fields) -- refreshed each cycle (not accumulating/scrolling), every
# INTERVAL seconds (default 1) until Ctrl+C. Built in rather than relying
# on the external "watch" command -- not available on Windows/Git Bash,
# which this has been run from all session. The whole frame is built into
# a variable and written in one atomic printf after the clear (not several
# separate ones), which is what makes a 1s refresh look stable rather than
# flickery.
#
# Usage: ./get-status.sh           -- one-shot, full JSON
#        ./get-status.sh -w        -- watch mode, 1s interval
#        ./get-status.sh -w 5      -- watch mode, 5s interval

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
  echo "Usage: ./get-status.sh [-w [INTERVAL_SEC]]"
  echo "  (no args)          one-shot, full GET /status JSON from both nodes"
  echo "  -w [INTERVAL_SEC]  watch mode: side-by-side table, refreshed every"
  echo "                     INTERVAL_SEC seconds (default 1) until Ctrl+C --"
  echo "                     includes time_since_change and last_backup rows"
  echo "                     per node, plus the actual error text below the"
  echo "                     table for any node whose last backup attempt failed"
  exit 0
fi

delim()
{
  echo "--------------------------------------------------------------------------------"
}

header()
{
  echo
  delim
  echo "$@"
  delim
  echo
}

ts() { date -u +"%Y-%m-%dT%H:%M:%SZ"; }

# FIELDS: the jq path/key pairs shown -- role, reachability, replication
# lag, and every "something's in progress" flag -- deliberately not the
# full payload, just what's worth watching live during a
# failover/maintenance test. Order here is display order.
FIELDS=(role postgres_reachable peer_reachable lag_bytes shutdown switchover maintenance failover_countdown backup_in_progress)

# extract_fields: prints "key<TAB>value" for each of FIELDS, or nothing at
# all if json isn't valid (caller falls back to "?" per field in that case
# via the associative array's own missing-key default).
extract_fields()
{
  local json="$1"
  echo "$json" | jq -e . >/dev/null 2>&1 || return 1
  echo "$json" | jq -r '
    [
      ["role", .role],
      ["postgres_reachable", (.postgres_reachable | tostring)],
      ["peer_reachable", (.peer_reachable | tostring)],
      ["lag_bytes", ((.replication_lag_bytes // 0) | tostring)],
      ["shutdown", (.shutdown_in_progress | tostring)],
      ["switchover", (.switchover_in_progress | tostring)],
      ["maintenance", (.maintenance_active | tostring)],
      ["failover_countdown", (.failover_countdown_active | tostring)],
      ["backup_in_progress", (.backup_in_progress | tostring)]
    ][] | @tsv
  '
}

# last_backup_attempt_info: prints "ok<TAB>epoch<TAB>error" from /status's
# last_backup_attempt_ok/last_backup_attempt_timestamp_seconds/
# last_backup_attempt_error ("true\t0\t" if json is invalid) --
# deliberately the *attempt* fields, not the success-only
# last_backup_timestamp_seconds: a failed attempt still advances these,
# which is exactly what makes "backups are currently failing" visible here
# instead of just looking like "hasn't run in a while" (see README.md's
# Backup section, "Is it actually working?"). Separate from
# extract_fields/FIELDS above since this feeds computed rows, not a value
# shown/compared as-is.
last_backup_attempt_info()
{
  local json="$1"
  echo "$json" | jq -e . >/dev/null 2>&1 || { printf 'true\t0\t\n'; return; }
  # NOT ".last_backup_attempt_ok // true" -- jq's "//" treats a real `false`
  # as falsy too, same as null/missing, which would silently turn an actual
  # failure into "ok". Only substitute the default on an actually-missing/
  # null field.
  echo "$json" | jq -r '[(.last_backup_attempt_ok | if . == null then true else . end), (.last_backup_attempt_timestamp_seconds // 0 | floor), (.last_backup_attempt_error // "")] | @tsv'
}

# fmt_backup_status: "never" if no attempt yet, "<ago>" if the last attempt
# succeeded, "FAILED <ago>" if it didn't -- the one-line answer to "is
# backup currently working".
fmt_backup_status()
{
  local ok="$1" ts="$2" now="$3"
  if [ "$ts" = "0" ]; then
    echo "never"
    return
  fi
  local ago
  ago="$(fmt_duration $((now - ts)))"
  if [ "$ok" = "true" ]; then
    echo "$ago"
  else
    echo "FAILED $ago"
  fi
}

# fmt_duration: seconds -> "45s" / "2m15s" / "3h07m" / "1d02h" -- for the
# time_since_change and last_backup rows below. Backups can legitimately be
# many hours or days apart (PG_GUARD_BACKUP_INTERVAL defaults to 24h), so
# this scales beyond the minutes range time_since_change alone would ever
# actually need.
fmt_duration()
{
  local s="$1"
  if [ "$s" -lt 60 ]; then
    echo "${s}s"
  elif [ "$s" -lt 3600 ]; then
    printf '%dm%02ds\n' $((s / 60)) $((s % 60))
  elif [ "$s" -lt 86400 ]; then
    printf '%dh%02dm\n' $((s / 3600)) $((s % 3600 / 60))
  else
    printf '%dd%02dh\n' $((s / 86400)) $((s % 86400 / 3600))
  fi
}

# snapshot_of_v0/v1: all of FIELDS' current values joined into one
# comparable string, in FIELDS order. Two near-identical functions instead
# of one taking the array name, to avoid depending on bash's nameref
# (declare -n, 4.3+) -- not worth the portability risk on top of everything
# else this session already found Windows/Git-Bash-specific.
snapshot_of_v0()
{
  local out=""
  for key in "${FIELDS[@]}"; do out+="${V0[$key]:-?}|"; done
  echo "$out"
}
snapshot_of_v1()
{
  local out=""
  for key in "${FIELDS[@]}"; do out+="${V1[$key]:-?}|"; done
  echo "$out"
}

if [ "${1:-}" = "-w" ]; then
  INTERVAL="${2:-1}"
  PREV0="" PREV1=""
  LAST_CHANGE0=$SECONDS
  LAST_CHANGE1=$SECONDS
  while true; do
    json0="$(curl -s --connect-timeout 4 http://localhost:9100/status)"
    json1="$(curl -s --connect-timeout 4 http://localhost:9101/status)"

    declare -A V0=() V1=()
    while IFS=$'\t' read -r key value; do V0["$key"]="$value"; done < <(extract_fields "$json0")
    while IFS=$'\t' read -r key value; do V1["$key"]="$value"; done < <(extract_fields "$json1")
    IFS=$'\t' read -r bk0ok bk0ts bk0err < <(last_backup_attempt_info "$json0")
    IFS=$'\t' read -r bk1ok bk1ts bk1err < <(last_backup_attempt_info "$json1")

    # Reset each node's own timer only if THAT node's snapshot actually
    # changed since last cycle -- the two nodes can settle at different
    # times, so this is tracked independently, not as one shared clock.
    snap0="$(snapshot_of_v0)"
    snap1="$(snapshot_of_v1)"
    [ -n "$PREV0" ] && [ "$snap0" != "$PREV0" ] && LAST_CHANGE0=$SECONDS
    [ -n "$PREV1" ] && [ "$snap1" != "$PREV1" ] && LAST_CHANGE1=$SECONDS
    PREV0="$snap0"
    PREV1="$snap1"

    # Build the whole frame into a variable first, then clear-screen and
    # write it in one single printf -- one atomic terminal write instead of
    # several separate ones after the clear, so there's no window where a
    # slow/janky terminal could show a half-drawn frame.
    frame="$( {
      echo "watching every ${INTERVAL}s -- Ctrl+C to stop -- $(ts)"
      echo
      printf '%-20s %-12s %-12s\n' "parameter" "t-0" "t-1"
      for key in "${FIELDS[@]}"; do
        printf '%-20s %-12s %-12s\n' "$key" "${V0[$key]:-?}" "${V1[$key]:-?}"
      done
      printf '%-20s %-12s %-12s\n' "time_since_change" \
        "$(fmt_duration $((SECONDS - LAST_CHANGE0)))" "$(fmt_duration $((SECONDS - LAST_CHANGE1)))"
      now_epoch="$(date +%s)"
      printf '%-20s %-12s %-12s\n' "last_backup" \
        "$(fmt_backup_status "$bk0ok" "$bk0ts" "$now_epoch")" "$(fmt_backup_status "$bk1ok" "$bk1ts" "$now_epoch")"
      # Free-text error line(s), not column-constrained like the table
      # above -- only printed for a node whose last attempt actually
      # failed, so a healthy run never has empty lines here.
      [ "$bk0ok" = "false" ] && [ -n "$bk0err" ] && echo "  t-0 last backup error: $bk0err"
      [ "$bk1ok" = "false" ] && [ -n "$bk1err" ] && echo "  t-1 last backup error: $bk1err"
    } )"
    # ANSI clear-screen + cursor-home, not the external "clear" command --
    # one less thing to depend on being installed/on PATH.
    printf '\033[2J\033[H%s\n' "$frame"
    sleep "$INTERVAL"
  done
else
  header "pg-traveler-0"
  curl -s --connect-timeout 4 http://localhost:9100/status|jq

  header "pg-traveler-1"
  curl -s --connect-timeout 4 http://localhost:9101/status|jq

  echo
fi
