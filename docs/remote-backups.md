# Keep world backups off the host

[Documentation](README.md) / Remote backups

Valmin copies completed world archives to one active destination. Choose WebDAV
for a direct connection, or rclone for Google Drive, S3-compatible storage,
OneDrive, and other configured services. Uploading never stops the game server.

## Configure WebDAV

1. Open **Remote backups** in the header's **Administration** menu.
2. Select **WebDAV** and enter the existing HTTPS collection URL, username, and
   app password. For Nextcloud, use the WebDAV URL shown in Files settings.
3. Enter a relative folder, such as `Backups`.
4. Save the destination and select **Test connection**. The test writes, checks,
   and deletes a uniquely named probe.
5. Enable the destination and save.

TLS certificates must be trusted by the panel. Redirects and credentials embedded
in URLs are rejected. Private network destinations, and carrier-grade NAT addresses
such as a Tailscale `100.x` address, require an operator-owned
`remote_backups.allowed_private_cidrs` allowlist. Loopback, link-local, metadata,
and multicast addresses remain blocked.

## Create a connection in the panel

The official panel image includes rclone. Standalone deployments must install it
and make it available to the panel process.

1. Open **Remote backups** in the header's **Administration** menu, choose **rclone**,
   and select **Create connection**.
2. Choose the provider and a connection name, then fill in its fields:
   - **Google Drive, Dropbox, OneDrive**: the client ID and secret of your own OAuth
     app. Register the callback URL the dialog shows, exactly as shown, then select
     **Connect account** and approve access at the provider.
   - **MEGA**: the account email and password.
   - **S3-compatible**: the provider name rclone uses (or `Other`), the HTTPS endpoint,
     the region, and the access keys. AWS needs no endpoint.
3. For OneDrive, choose the drive that stores the backups.
4. Select **Save connection**. The new connection is selected in the destination form;
   set the folder, save, test the connection, and enable the destination.

You can save several connections; only the one selected as the destination
receives backups. Unfinished setups expire after 15 minutes, are tied to
the browser session that started them, and do not survive a panel restart.

OAuth providers accept only HTTPS callback URLs, or `http://localhost`. Set each app up
as follows:

- **Google**: create an OAuth client of type *Web application* and add the callback
  URL as an authorized redirect URI. Google does not accept an IP address there, so
  reach the panel by domain name. Publish the app: while its status is *Testing*,
  Google's authorization expires after seven days.
- **Dropbox**: create an app, add the callback URL as a redirect URI, and enable the
  `files.metadata.write`, `files.content.write`, `files.content.read`, `sharing.write`
  and `account_info.read` permissions. **App Folder** access keeps backups separate.
- **Microsoft**: register an app that allows personal Microsoft accounts, add the
  callback URL as a *Web* redirect URI, and create a client secret.

The panel writes the connection to its rclone configuration, which it creates with
directory mode `0700` and file mode `0600` when missing. It refuses an encrypted
rclone configuration. An S3 endpoint must resolve to a public address, or to a
private or carrier-grade NAT network listed in `remote_backups.allowed_private_cidrs`. This is checked when
the setup starts; rclone resolves the name again on every transfer.

## Configure an rclone account

To create a remote by hand instead:

1. Use `rclone config` to create a named remote. Examples in
   [`deploy/rclone/`](../deploy/rclone/README.md) cover Drive, Dropbox, MEGA, S3, and OneDrive.
2. For OAuth services on a headless host, authorize on a computer with a browser
   using the same rclone version. Follow [rclone's headless setup guide](https://rclone.org/remote_setup/).
3. Put the resulting configuration at `<data.root>/rclone/rclone.conf`. The panel
   user must own the directory and file. Use directory mode `0700` and file mode
   `0600`. Both must remain writable: rclone refreshes tokens and replaces the
   configuration atomically. Never commit authenticated configuration.
4. Open **Remote backups** in the **Administration** menu, choose **rclone**, and
   select **Load configured remotes**. Enter the remote name without a colon.
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

| Key                                    | Default                          |
| -------------------------------------- | -------------------------------- |
| `remote_backups.rclone_binary`         | `rclone` from the daemon's PATH  |
| `remote_backups.rclone_config`         | `<data.root>/rclone/rclone.conf` |
| `remote_backups.allowed_private_cidrs` | Empty                            |

These use the existing YAML, environment, and flag precedence. Environment names
include `VALMIN_REMOTE_BACKUPS_RCLONE_CONFIG` and
`VALMIN_REMOTE_BACKUPS_ALLOWED_PRIVATE_CIDRS`. The API cannot set an executable,
configuration-file path, arbitrary command, or command-line flags.

## Enable uploads and retention

On each server's **Backups** page, enable **Automatically upload new backups**.
The switch and the retention fields stay disabled until a destination is configured.
This includes future manual, scheduled, and safety snapshots. Existing archives
are sent only with **Upload now**.

Remote retention defaults to 10 consistent copies, 5 best-effort copies, and 10 safety
snapshots. Each class is counted separately; zero keeps all copies in that class.
Retention runs after successful uploads and daily. Only recorded Valmin objects
are deleted, and they are deleted outright: on Google Drive, MEGA and OneDrive, pruned
copies skip the trash, so they stop counting against the quota at once. Raising a count
before cleanup runs keeps copies that were already marked. An upload that failed or was
cancelled after its archive reached the destination has that archive removed too. A
remote cleanup failure does not erase the upload success.

**Upload now** refuses an archive that retention would delete as soon as it arrived,
because the destination already keeps as many newer copies of that class as the server
allows. Raise the count first to keep it.

Changing the retention counts, or turning automatic uploads off, needs the
`backups.restore` capability, since lowering a count deletes copies.

Local deletion never deletes a remote copy. Pending uploads protect their local
archive for up to 24 hours. Success, cancellation, permanent failure, or expiration
releases that protection after the transfer stops. After release, normal local
retention applies; retry is possible only while the local archive still exists.

Transient failures retry with jittered exponential backoff, capped at one hour.
An attempt may run until the 24-hour retry window ends, so a large archive on a slow
uplink can finish in one attempt; an attempt that stops making progress for two minutes
ends and is retried. Failures rclone does not classify count as transient.
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
