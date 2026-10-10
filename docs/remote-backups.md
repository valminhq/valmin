# Keep world backups off the host

Valmin copies completed world archives to one active destination. Choose WebDAV
for a direct connection, or rclone for Google Drive, S3-compatible storage,
OneDrive, and other configured services. Uploading never stops the game server.

## Configure WebDAV

1. Open **Admin → Remote backups**.
2. Select **WebDAV** and enter the existing HTTPS collection URL, username, and
   app password. For Nextcloud, use the WebDAV URL shown in Files settings.
3. Enter a relative folder, such as `Backups`.
4. Save the destination and select **Test connection**. The test writes, checks,
   and deletes a uniquely named probe.
5. Enable the destination and save.

TLS certificates must be trusted by the panel. Redirects and credentials embedded
in URLs are rejected. Private network destinations require an operator-owned
`remote_backups.allowed_private_cidrs` allowlist. Loopback, link-local, metadata,
and multicast addresses remain blocked.

## Configure an rclone account

The official panel image includes rclone. Standalone deployments must install it
and make it available to the panel process.

1. Use `rclone config` to create a named remote. Examples in
   [`deploy/rclone/`](../deploy/rclone/README.md) cover Drive, Dropbox, MEGA, S3, and OneDrive.
2. For OAuth services on a headless host, authorize on a computer with a browser
   using the same rclone version. Follow [rclone's headless setup guide](https://rclone.org/remote_setup/).
3. Put the resulting configuration at `<data.root>/rclone/rclone.conf`. The panel
   user must own the directory and file. Use directory mode `0700` and file mode
   `0600`. Both must remain writable: rclone refreshes tokens and replaces the
   configuration atomically. Never commit authenticated configuration.
4. Open **Admin → Remote backups**, choose **rclone**, and select **Load configured
   remotes**. Enter the remote name without a colon.
5. Set the relative destination folder. For S3, include the bucket, for example
   `my-backup-bucket/valheim`.
6. Save, test the connection, and enable the destination.

Google Drive requires your own Google OAuth client credentials. The shared rclone
client is being retired during 2026. Use `drive.file` scope and let rclone create
the backup folder; this scope cannot see folders created outside that app.
Archives remain visible through Drive's website.
See [rclone's Drive guide](https://rclone.org/drive/).

Templates do not replace authorization. Tokens and passwords must come from your
account. An expired or revoked authorization requires reconnecting through rclone.

Operator configuration keys:

| Key | Default |
| --- | --- |
| `remote_backups.rclone_binary` | `rclone` from the daemon's PATH |
| `remote_backups.rclone_config` | `<data.root>/rclone/rclone.conf` |
| `remote_backups.allowed_private_cidrs` | Empty |

These use the existing YAML, environment, and flag precedence. Environment names
include `VALMIN_REMOTE_BACKUPS_RCLONE_CONFIG` and
`VALMIN_REMOTE_BACKUPS_ALLOWED_PRIVATE_CIDRS`. The API cannot set an executable,
configuration-file path, arbitrary command, or command-line flags.

## Enable uploads and retention

On each server's **Backups** page, enable **Automatically upload new backups**.
This includes future manual, scheduled, and safety snapshots. Existing archives
are sent only with **Upload now**.

Remote retention defaults to 10 consistent copies, 5 best-effort copies, and 10 safety
snapshots. Each class is counted separately; zero keeps all copies in that class.
Retention runs after successful uploads and daily. Only recorded Valmin objects
are deleted. A remote cleanup failure does not erase the upload success.

Local deletion never deletes a remote copy. Pending uploads protect their local
archive for up to 24 hours. Success, cancellation, permanent failure, or expiration
releases that protection after the transfer stops. After release, normal local
retention applies; retry is possible only while the local archive still exists.

Transient failures retry with jittered exponential backoff, capped at one hour.
Each attempt has a one-hour limit. The total automatic retry window is 24 hours.
The queue survives panel restarts. **Last successful copy** records transfer
completion; archive creation time shows how recent the backed-up world is.
Size confirmation is not a full remote checksum verification.

Disabling a destination pauses new uploads and cleanup. Existing pending copies
still expire after 24 hours. Changing connection identity or folder creates a new
namespace and cancels pending copies; active remote jobs must finish first.
Previous remote files remain and are no longer pruned by Valmin. Deleting a server
also leaves remote files intact; cancel outstanding uploads before deleting it.

## Recover after host loss

1. Download the chosen `.tar.gz` archive and matching `.tar.gz.json` manifest
   through the storage provider or rclone.
2. Compare the archive's SHA-256 with the manifest's `sha256` field.
3. Extract the archive into a temporary directory. The manifest records server,
   world name, creation time, and whether the archive is consistent.
4. Create a server in a fresh Valmin installation and leave it stopped.
5. Import the extracted world files or a ZIP containing those files through world
   import. The importer does not directly accept `.tar.gz`.
6. Restore mods and server configuration separately, then start the server.

The archive contains existing world-backup content. It does not protect the panel
database, accounts, secret key, installed mods, or deployment configuration.
Keep a separate [whole-installation backup](operations.md#back-up-the-whole-installation).
A hot copy remains best-effort even after uploading.

## Verify an installation

Run checks only through Makefile targets:

- `make fmt`, then `make lint`.
- `make test-go` and `make test-web`.
- `make test-remote-race` for queue, worker, and provider concurrency checks.
- `make test-rclone-templates` to parse the example configurations offline.
- `make test-remote-integration` for the real rclone adapter against a local TLS
  WebDAV server.
- `make test-remote-smoke REMOTE_SMOKE_REMOTE=drive-backups REMOTE_SMOKE_CONFIG=/absolute/path/rclone.conf`
  for a configured account. Optionally set `REMOTE_SMOKE_FOLDER`, such as an S3
  bucket prefix. Repeat for each configured provider.

Smoke tests create and delete only uniquely named test objects. They do not
modify an existing backup. Without account configuration, cloud smoke tests skip
with an explicit message.
