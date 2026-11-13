#!/bin/sh
# Runs backup.sh every day at BACKUP_AT (UTC, default 03:00), and once at
# start so a deploy shows at once whether backups work. `run.sh once` takes a
# single backup and exits.
set -eu

if [ "${1:-}" = "once" ]; then
  exec /usr/local/bin/backup.sh
fi

at="${BACKUP_AT:-03:00}"
if [ "${BACKUP_ON_START:-true}" = "true" ]; then
  /usr/local/bin/backup.sh || true
fi
while true; do
  now=$(date -u +%s)
  next=$(date -u -d "$(date -u +%Y-%m-%d) $at:00" +%s)
  if [ "$next" -le "$now" ]; then
    next=$((next + 86400))
  fi
  echo "next backup at $(date -u -d "@$next" '+%Y-%m-%d %H:%M UTC')"
  sleep $((next - now))
  # A failed night is reported by backup.sh; the loop carries on regardless.
  /usr/local/bin/backup.sh || true
done
