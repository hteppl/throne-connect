# Registers throne-connect as a hidden task that starts at logon and restarts on failure.
# Run from the folder that holds throne-connect.exe and xray.exe:
#   powershell -ExecutionPolicy Bypass -File install-windows.ps1
$ErrorActionPreference = "Stop"
$exe = Join-Path $PSScriptRoot "throne-connect.exe"
if (-not (Test-Path $exe)) { $exe = Join-Path (Get-Location) "throne-connect.exe" }
if (-not (Test-Path $exe)) { throw "throne-connect.exe not found" }

$action = New-ScheduledTaskAction -Execute $exe -Argument "run" -WorkingDirectory (Split-Path $exe)
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME
$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
    -ExecutionTimeLimit ([TimeSpan]::Zero) -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) -Hidden
Register-ScheduledTask -TaskName "throne-connect" -Action $action -Trigger $trigger -Settings $settings -Force | Out-Null
Start-ScheduledTask -TaskName "throne-connect"
Write-Host "throne-connect installed and started. Remove with: Unregister-ScheduledTask throne-connect"
