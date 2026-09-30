# Configure a backup remote

These examples describe rclone configuration fields. They contain no working
credentials. Complete setup with `rclone config --config /absolute/path/rclone.conf`
and install the resulting file as described in the [backup guide](../../docs/remote-backups.md).
Keep the directory private and writable by the panel user (0700), with the file
at 0600. For headless OAuth, follow the authorization command printed by rclone
on a computer with a browser and the same rclone version.

## Choose a template

| Provider | Template | Remote name in Valmin | Example folder in Valmin |
| --- | --- | --- | --- |
| Google Drive | [drive.conf.example](drive.conf.example) | `drive-backups` | `ValminBackups` |
| Dropbox | [dropbox.conf.example](dropbox.conf.example) | `dropbox-backups` | `ValminBackups` |
| MEGA | [mega.conf.example](mega.conf.example) | `mega-backups` | `ValminBackups` |
| S3-compatible | [s3.conf.example](s3.conf.example) | `s3-backups` | `your-bucket/valmin` |
| OneDrive | [onedrive.conf.example](onedrive.conf.example) | `onedrive-backups` | `ValminBackups` |

Select **rclone** in **Admin → Remote backups**, load the configured remotes, and
enter the remote name and relative folder separately. Save and test the connection
before enabling automatic uploads on a server.

## Set up Google Drive

Create your own OAuth client and enter its ID and secret in `rclone config`.
Select `drive.file` scope, complete browser authorization, and let rclone create
the destination folder. That scope cannot access folders created outside the app;
its backups remain visible in Drive's website. The template retains Drive's trash
behavior, so pruning does not imply immediate storage reclamation.
Follow the [official Drive setup](https://rclone.org/drive/).

## Set up Dropbox

Select `dropbox` in `rclone config` and complete browser authorization. Default
client credentials can remain blank. For a separate backup area, create your own
Dropbox app with **App Folder** access and configure its client ID and secret.
The template uses `batch_mode = sync` to wait for upload completion. Use a personal
or app-folder path; Valmin does not accept leading-slash team-folder paths.
Follow the [official Dropbox setup](https://rclone.org/dropbox/).

## Set up MEGA

Log into MEGA through its website once to initialize the account's encryption
keys. Select `mega` in `rclone config`, then enter your email and password at the
prompts. Let rclone generate the obscured `pass` field; obscuring is reversible
and does not replace file permissions. The template enables HTTPS and retains trash.

Rclone exposes a one-time `2fa` code. Because Valmin starts a new rclone process
for each operation, a saved code is not a persistent authentication solution.
Treat unattended use with a 2FA-enabled account as unverified. MEGA also documents
login blocking after rapid repeated commands. Test your account before relying
on scheduled copies. See the [official MEGA setup and limitations](https://rclone.org/mega/).

## Validate the setup

Run `make test-rclone-templates` to parse all example configurations with the
installed rclone. This checks configuration syntax and remote names, without
logging into cloud accounts. Run `make test-remote-integration` for an actual
rclone upload, metadata read, and deletion against a temporary TLS WebDAV server.

For a configured account, run the existing smoke target with its name and private
configuration path. For example:

```sh
make test-remote-smoke REMOTE_SMOKE_REMOTE=dropbox-backups REMOTE_SMOKE_CONFIG=/absolute/path/rclone.conf REMOTE_SMOKE_FOLDER=ValminBackups
```

The smoke test creates and deletes only a unique probe. Repeat it with
`drive-backups` or `mega-backups` after authorizing those accounts.
