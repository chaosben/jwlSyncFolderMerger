# systemd (Linux)

Runs `jwlSyncFolderMerger -watch` as a **user service**. The unit is a template:
the instance name selects an environment file, so several backup folders can be
served in parallel.

## Install

```sh
install -Dm755 jwlSyncFolderMerger ~/.local/bin/jwlSyncFolderMerger
install -Dm644 jwlSyncFolderMerger-watch@.service ~/.config/systemd/user/jwlSyncFolderMerger-watch@.service
install -Dm600 example.env ~/.config/jwlSyncFolderMerger/jwlibrary.env
$EDITOR ~/.config/jwlSyncFolderMerger/jwlibrary.env      # set JWLSFM_DIR and JWLSFM_SYNC

systemctl --user daemon-reload
systemctl --user enable --now jwlSyncFolderMerger-watch@jwlibrary
```

Keep running without an active login session:

```sh
loginctl enable-linger "$USER"
```

## Operate

```sh
systemctl --user status jwlSyncFolderMerger-watch@jwlibrary
journalctl --user -u jwlSyncFolderMerger-watch@jwlibrary -f
```

`JWLSFM_ARGS=-log-format json` gives structured logs in the journal. A run aborted by
the mass-deletion guard is logged as an error; the service keeps running and tries
again when the next backup arrives.

## Environment file

| Variable      | Meaning                                              |
|---------------|------------------------------------------------------|
| `JWLSFM_DIR`  | backup folder (required)                             |
| `JWLSFM_SYNC` | `generic` or `syncthing` (required)                  |
| `JWLSFM_ARGS` | further flags, e.g. `-debounce 30s -poll 1h`         |

If the folder lies on a filesystem without change events (network mounts, a Windows
drive under WSL), the service falls back to polling (`-poll`, default 5 minutes).
