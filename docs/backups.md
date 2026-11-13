# Database backups

Every night at 03:00 UTC (and whenever the accessory starts), the `backup` accessory dumps the production database, compresses it and encrypts it with [age](https://age-encryption.org), then uploads it to the private Backblaze B2 bucket `visicomms-backup` under `db/`. Copies older than 31 days are deleted; for their first 30 days Object Lock keeps anyone, this server included, from deleting them.

| Piece | Where |
| --- | --- |
| Image | `deploy/backup`, published as `ghcr.io/regatlas-llc/visicomms-backup:1` by `.github/workflows/backup-image.yml` |
| Schedule and settings | `accessories.backup` in `config/deploy.yml` |
| Public key (encrypts) | `BACKUP_AGE_RECIPIENT` in `config/deploy.yml`; safe to publish |
| Private key (decrypts) | Your password manager. Never on the server |
| B2 credentials | `B2_BACKUP_KEY_ID`, `B2_BACKUP_APPLICATION_KEY` in `.kamal/secrets`: an application key limited to the bucket |
| Missed-backup alert | `BACKUP_HEARTBEAT_URL` in `.kamal/secrets` (optional) |

The server only has the public key. Someone who takes over the server can't read any backup, and neither can someone who reads the bucket.

## One-time setup

1. **Bucket.** In Backblaze B2, a **private** bucket named `visicomms-backup` with **Object Lock enabled** and a default retention of **Governance, 30 days**: every backup is locked until it is old enough to prune, so a stolen key can't delete recent backups. Done on 2026-10-08.
2. **Key.** The website's "Read and Write" preset includes `bypassGovernance` and retention writes, which would let the key undo the lock. Create a narrower key with the `b2` CLI and your master key instead:

   ```bash
   brew install b2-tools
   b2 account authorize            # master application key
   b2 key create --bucket visicomms-backup visicomms-backup-writer listBuckets,listFiles,readFiles,writeFiles,deleteFiles
   b2 account clear
   ```

   Save the keyID and applicationKey in `VisiComms/Production` as `B2_BACKUP_KEY_ID` and `B2_BACKUP_APPLICATION_KEY`. Uploads go through B2's S3 API (`s3.us-west-004.backblazeb2.com`): B2's native API v1, which rclone's B2 backend uses, refuses keys made by the current CLI.

   In the bucket's **Lifecycle Settings**, choose **Keep only the last version of the file**. Over S3 a delete only hides a file, and this rule purges hidden files a day later. Without it, pruned backups would stay stored and billed. Done on 2026-10-08.
3. **Missed-backup alert (recommended).** Create a free check at healthchecks.io: period 1 day, grace 2 hours. Save its ping URL as `BACKUP_HEARTBEAT_URL`. Every successful backup pings it; a failure pings `<url>/fail` with the reason. If no ping arrives, healthchecks.io emails you. That covers a backup that quietly stops running.
4. **Start it.**

   ```bash
   export VISICOMMS_HOST=<server ip> VISICOMMS_DOMAIN=<domain>
   kamal accessory boot backup        # takes a backup right away
   kamal accessory logs backup        # expect "uploaded visicomms_...sql.gz.age"
   ```

## Restoring

Restore into an **empty** database. Never restore over the live one; restore beside it, check it, then switch.

```bash
umask 077
op document get "VisiComms backup key (age)" --vault VisiComms > /tmp/visicomms.agekey

docker run --rm -v /tmp/visicomms.agekey:/key:ro \
  -e BACKUP_AGE_IDENTITY_FILE=/key \
  -e BACKUP_DEST=backup:visicomms-backup/db \
  -e RCLONE_CONFIG_BACKUP_TYPE=s3 -e RCLONE_CONFIG_BACKUP_PROVIDER=Other \
  -e RCLONE_CONFIG_BACKUP_ENDPOINT=https://s3.us-west-004.backblazeb2.com \
  -e RCLONE_CONFIG_BACKUP_ACCESS_KEY_ID=$(op read op://<vault>/<item>/B2_BACKUP_KEY_ID) \
  -e RCLONE_CONFIG_BACKUP_SECRET_ACCESS_KEY=$(op read op://<vault>/<item>/B2_BACKUP_APPLICATION_KEY) \
  -e PGHOST=<target host> -e PGUSER=<user> -e PGPASSWORD=<password> -e PGDATABASE=<empty database> \
  --entrypoint restore.sh ghcr.io/regatlas-llc/visicomms-backup:1 latest

rm /tmp/visicomms.agekey
```

Pass a file name instead of `latest` for an older copy. List them with `rclone lsf` against the same remote, or in the B2 dashboard.

**Restore drill.** A backup nobody has restored is a hope. Once a month, restore `latest` into a throwaway local Postgres (`docker run -e POSTGRES_PASSWORD=x -p 55432:5432 postgres:17`) and check that it holds what production does. This was rehearsed on 2026-10-08 against the demo data: every table's row count matched, and the append-only rule on events survived.
