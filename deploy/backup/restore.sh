#!/bin/sh
# Restores one backup into an empty database. Needs the age private key,
# which never lives on the server: run this from a machine with 1Password.
#
#   restore.sh <file name | latest>
#
# Environment: BACKUP_DEST and the rclone remote as for backup.sh, PG* for the
# target database, and BACKUP_AGE_IDENTITY_FILE pointing at the private key.
set -eu -o pipefail

: "${BACKUP_AGE_IDENTITY_FILE:?set BACKUP_AGE_IDENTITY_FILE}"
name="${1:?usage: restore.sh <file name | latest>}"
if [ "$name" = "latest" ]; then
  name=$(rclone lsf --files-only "$BACKUP_DEST" | grep '\.sql\.gz\.age$' | sort | tail -1)
  [ -n "$name" ] || { echo "no backups in $BACKUP_DEST" >&2; exit 1; }
fi
echo "restoring $name into $PGDATABASE on $PGHOST"
rclone cat "$BACKUP_DEST/$name" | age -d -i "$BACKUP_AGE_IDENTITY_FILE" | gunzip | psql -v ON_ERROR_STOP=1 -q >/dev/null
echo "restored $name"
