# Downloads the latest xray-core for Windows next to throne-connect.exe (or into -Dest).
param([string]$Dest = ".")
$ErrorActionPreference = "Stop"
$asset = if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64" -or $env:PROCESSOR_ARCHITEW6432 -eq "ARM64") { "Xray-windows-arm64-v8a.zip" }
    elseif ([Environment]::Is64BitOperatingSystem) { "Xray-windows-64.zip" }
    else { "Xray-windows-32.zip" }
$url = "https://github.com/XTLS/Xray-core/releases/latest/download/$asset"
$tmp = Join-Path $env:TEMP "xray-$([guid]::NewGuid()).zip"
$dir = Join-Path $env:TEMP "xray-$([guid]::NewGuid())"
Write-Host "downloading $url"
Invoke-WebRequest -Uri $url -OutFile $tmp -UseBasicParsing
Expand-Archive -Path $tmp -DestinationPath $dir
New-Item -ItemType Directory -Force -Path $Dest | Out-Null
Copy-Item (Join-Path $dir "xray.exe") -Destination $Dest -Force
Remove-Item $tmp, $dir -Recurse -Force
& (Join-Path $Dest "xray.exe") version | Select-Object -First 1
