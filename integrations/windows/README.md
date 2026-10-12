# Windows

Runs `jwlSyncFolderMerger.exe -watch` as a scheduled task that starts at log on,
runs without a console window and restarts after failures.

## Install

1. Copy `jwlSyncFolderMerger.exe` to `%LOCALAPPDATA%\jwlSyncFolderMerger\`.
2. In PowerShell (no administrator rights needed for your own user in most setups):

   ```powershell
   .\install-task.ps1 -Folder "C:\Users\me\Sync\JWLibrary"
   Start-ScheduledTask -TaskName "jwlSyncFolderMerger watch"
   ```

   Options: `-Sync generic|syncthing` (default `syncthing`), `-ExtraArgs "-debounce 30s"`,
   `-Exe <path>`, `-TaskName <name>`.

   If script execution is blocked: `powershell -ExecutionPolicy Bypass -File .\install-task.ps1 …`

## Updates

The task installs new releases by itself (see "Automatic updates" in the main
README): `%LOCALAPPDATA%\jwlSyncFolderMerger` is writable for your user. Windows cannot
replace a running program, so the old executable is renamed to
`jwlSyncFolderMerger.exe.old-…` and stays as a small waiting parent process of the new
version until the task ends; leftover `.old-…` files are removed at the next start.
Stopping the task stops both processes. To turn updates off, install the task with
`-ExtraArgs "-no-auto-update"`.

## Operate

- Log: `%LOCALAPPDATA%\jwlSyncFolderMerger\watch.log`
- Status: `Get-ScheduledTask -TaskName "jwlSyncFolderMerger watch" | Get-ScheduledTaskInfo`
- Stop / remove: `Stop-ScheduledTask …` / `Unregister-ScheduledTask -TaskName "jwlSyncFolderMerger watch"`

The task uses the logon type S4U ("run whether user is logged on or not" without
storing a password), so no window appears. Local folders in your profile are
accessible; network shares are not.
