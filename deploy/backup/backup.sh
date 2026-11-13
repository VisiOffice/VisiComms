#!/bin/sh
# One backup: pg_dump | gzip | age (public-key encryption), uploaded with
# rclone, then copies older than BACKUP_KEEP_DAYS are deleted. Only the
# recipient (public) key is here, so this host cannot read its own backups.
#
# Environment:
#   PGHOST PGUSER PGPASSWORD PGDATABASE   the database to dump
#   BACKUP_AGE_RECIPIENT                  age public key (age1...)
#   BACKUP_DEST                           rclone path, e.g. backup:visicomms-backup/db (B2)
#   RCLONE_CONFIG_BACKUP_*                the "backup" rclone remote (see docs/backups.md)
#   BACKUP_KEEP_DAYS                      default 30
#   HEARTBEAT_URL                         optional: pinged on success, <url>/fail on failure
set -eu -o pipefail

name="visicomms_$(date -u +%Y%m%d_%H%M%S).sql.gz.age"
tmp="/tmp/$name"
trap 'rm -f "$tmp"' EXIT

fail() {
  echo "backup failed: $1" >&2
  if [ -n "${HEARTBEAT_URL:-}" ]; then
    curl -fsS -m 10 --retry 3 --data-raw "$1" "$HEARTBEAT_URL/fail" >/dev/null || true
  fi
  exit 1
}

: "${BACKUP_AGE_RECIPIENT:?set BACKUP_AGE_RECIPIENT}"
: "${BACKUP_DEST:?set BACKUP_DEST}"

echo "dumping $PGDATABASE on $PGHOST"
if ! pg_dump --no-owner --no-acl | gzip -9 | age -r "$BACKUP_AGE_RECIPIENT" -o "$tmp"; then
  fail "pg_dump, gzip or age exited with an error"
fi
size=$(wc -c < "$tmp")
[ "$size" -gt 0 ] || fail "the encrypted dump is empty"

rclone copyto "$tmp" "$BACKUP_DEST/$name" || fail "upload to $BACKUP_DEST failed"
echo "uploaded $name ($size bytes)"

rclone delete --min-age "${BACKUP_KEEP_DAYS:-30}d" "$BACKUP_DEST" || echo "warning: could not prune old backups" >&2

if [ -n "${HEARTBEAT_URL:-}" ]; then
  curl -fsS -m 10 --retry 3 "$HEARTBEAT_URL" >/dev/null || echo "warning: heartbeat ping failed" >&2
fi
