# macOS (launchd)

Runs `jwlSyncFolderMerger -watch` as a launch agent: it starts at log on and is
restarted after errors.

## Install

1. Put the executable in place and allow it to run:

   ```sh
   mkdir -p ~/bin && cp jwlSyncFolderMerger ~/bin/
   xattr -d com.apple.quarantine ~/bin/jwlSyncFolderMerger
   ```

2. Copy the agent and replace `/Users/me/…` with your paths (executable, backup
   folder, log file); use `-sync generic` for sync tools other than Syncthing:

   ```sh
   cp com.github.chaosben.jwlSyncFolderMerger.plist ~/Library/LaunchAgents/
   open -e ~/Library/LaunchAgents/com.github.chaosben.jwlSyncFolderMerger.plist
   plutil -lint ~/Library/LaunchAgents/com.github.chaosben.jwlSyncFolderMerger.plist
   ```

3. Load it:

   ```sh
   launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.github.chaosben.jwlSyncFolderMerger.plist
   ```

If the backup folder is in a protected location (Desktop, Documents, iCloud
Drive), macOS asks for permission on first access, or you grant the executable
"Full Disk Access" in System Settings → Privacy & Security.

## Updates

The agent installs new releases by itself (see "Automatic updates" in the main
README): `~/bin` is writable for your user, and the new version continues in the
same process. To turn this off, add `<string>-no-auto-update</string>` to
`ProgramArguments`.

## Operate

- Log: `~/Library/Logs/jwlSyncFolderMerger.log`
- Status: `launchctl print gui/$(id -u)/com.github.chaosben.jwlSyncFolderMerger`
- Stop and remove:

  ```sh
  launchctl bootout gui/$(id -u)/com.github.chaosben.jwlSyncFolderMerger
  rm ~/Library/LaunchAgents/com.github.chaosben.jwlSyncFolderMerger.plist
  ```
