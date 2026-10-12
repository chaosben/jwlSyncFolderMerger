# Integrations

There are two kinds of integrations:

1. **Sync adapters** (code, `internal/syncadapter`): what the tool needs to know
   about a sync tool – its temporary/conflict files, when a file is completely
   synchronised and, optionally, how to get notified about changes.
2. **Service configurations** (this directory): how to keep
   `jwlSyncFolderMerger -watch` running on an operating system.

| Directory              | Content                                                   |
|------------------------|-----------------------------------------------------------|
| [systemd/](systemd/)   | user service template for Linux                           |
| [windows/](windows/)   | scheduled task (at log on, no window, restart on failure) |
| [macos/](macos/)       | launchd agent (at log on, restart on failure)             |

All of them install the executable in a directory owned by the user, so the watch
mode can update itself (see "Automatic updates" in the main README).

## Sync adapters

| `-sync`     | Change notification          | Notes                                   |
|-------------|------------------------------|-----------------------------------------|
| `generic`   | filesystem events (fsnotify) | any sync tool; temp names `*.tmp`, `*.part`, `.*`, `~*` are ignored |
| `syncthing` | filesystem events (fsnotify) | additionally ignores `.syncthing.*`, `~syncthing~*`; conflict copies are inputs |

Conflict copies created by a sync tool are regular inputs: identical copies are
detected by content hash and removed.

### Adding a sync tool

1. Add `internal/syncadapter/<tool>.go` with a type implementing `Adapter`
   (usually embed `generic` and extend `Ignore`).
2. Optional capability `Watcher` (`Watch(ctx, dir, changed)`): implement it to use
   the tool's own notification mechanism (e.g. an events API); otherwise reuse
   `watchDir` with the adapter's `Ignore`.
3. Register it in `registry` – it can then be selected with `-sync <tool>`.
4. Settings of an adapter come from environment variables named
   `JWLSFM_<TOOL>_<SETTING>` (e.g. `JWLSFM_SYNCTHING_API_KEY`); never log secrets.
5. Add tests (`syncadapter_test.go`, and a fake server for API-based watchers).
6. Optionally add `integrations/<tool>/README.md` with recommended settings.
