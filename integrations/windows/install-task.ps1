<#
.SYNOPSIS
    Registers a scheduled task that runs jwlSyncFolderMerger in watch mode.

.DESCRIPTION
    The task starts at log on, runs without a console window (logon type S4U) and
    restarts after failures. Log output is appended to
    %LOCALAPPDATA%\jwlSyncFolderMerger\watch.log.

.EXAMPLE
    .\install-task.ps1 -Folder "C:\Users\me\Sync\JWLibrary"
#>
param(
    [Parameter(Mandatory = $true)]
    [string]$Folder,
    [string]$Exe = (Join-Path $env:LOCALAPPDATA "jwlSyncFolderMerger\jwlSyncFolderMerger.exe"),
    [ValidateSet("generic", "syncthing")]
    [string]$Sync = "syncthing",
    [string]$ExtraArgs = "",
    [string]$TaskName = "jwlSyncFolderMerger watch"
)

$ErrorActionPreference = "Stop"

if (-not (Test-Path -LiteralPath $Exe)) { throw "Executable not found: $Exe" }
if (-not (Test-Path -LiteralPath $Folder)) { throw "Folder not found: $Folder" }

$logDir = Join-Path $env:LOCALAPPDATA "jwlSyncFolderMerger"
New-Item -ItemType Directory -Force -Path $logDir | Out-Null
$log = Join-Path $logDir "watch.log"

# cmd.exe only serves to redirect the log output to a file.
$command = "`"`"$Exe`" -watch -sync $Sync $ExtraArgs `"$Folder`" 2>> `"$log`"`""
$action = New-ScheduledTaskAction -Execute "cmd.exe" -Argument "/c $command"
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME
$settings = New-ScheduledTaskSettingsSet `
    -ExecutionTimeLimit ([TimeSpan]::Zero) `
    -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) `
    -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
    -MultipleInstances IgnoreNew
$principal = New-ScheduledTaskPrincipal -UserId $env:USERNAME -LogonType S4U -RunLevel Limited

Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger `
    -Settings $settings -Principal $principal -Force | Out-Null

Write-Host "Registered task '$TaskName'. Start it now with: Start-ScheduledTask -TaskName '$TaskName'"
Write-Host "Log file: $log"
