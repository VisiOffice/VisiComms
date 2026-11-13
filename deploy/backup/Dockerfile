# Nightly encrypted Postgres backups, run by Kamal as the "backup" accessory.
# Kept apart from the app image, which holds nothing but the Go binary.
FROM alpine:3.22
RUN apk add --no-cache postgresql17-client age rclone curl tzdata
COPY backup.sh restore.sh run.sh /usr/local/bin/
RUN chmod +x /usr/local/bin/*.sh && adduser -D -H backup
# rclone needs no config file: its remote is set through RCLONE_CONFIG_BACKUP_* env.
ENV RCLONE_CONFIG=/tmp/rclone.conf
USER backup
ENTRYPOINT ["/usr/local/bin/run.sh"]
