# Installs MineNest for this Windows user (no administrator needed):
#   irm https://github.com/OwenCosta/minenest/releases/latest/download/install.ps1 | iex
# It puts minenest.exe and minenest-gui.exe in %LOCALAPPDATA%\Programs\MineNest, adds that folder
# to this user's PATH, makes a Start menu shortcut, and opens MineNest. Each file is checked
# against the release's SHA256SUMS before it's kept.

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$repo = 'OwenCosta/minenest'
$base = "https://github.com/$repo/releases/latest/download"
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
$dir = Join-Path $env:LOCALAPPDATA 'Programs\MineNest'

Write-Host 'Installing MineNest...'
New-Item -ItemType Directory -Force -Path $dir | Out-Null
$sums = @{}
foreach ($line in (Invoke-WebRequest -UseBasicParsing "$base/SHA256SUMS").Content -split "`n") {
    $f = $line.Trim() -split '\s+'
    if ($f.Count -eq 2) { $sums[$f[1].TrimStart('*')] = $f[0].ToLower() }
}
# A running MineNest keeps its files open: stop it first (its servers save and stop).
$old = Join-Path $dir 'minenest.exe'
if (Test-Path $old) { try { & $old quit | Out-Null; Start-Sleep -Seconds 5 } catch {} }

foreach ($pair in @(@("minenest-windows-$arch.exe", 'minenest.exe'), @("minenest-gui-windows-$arch.exe", 'minenest-gui.exe'))) {
    $asset, $name = $pair
    $tmp = Join-Path $dir "$name.download"
    Invoke-WebRequest -UseBasicParsing "$base/$asset" -OutFile $tmp
    $got = (Get-FileHash -Algorithm SHA256 $tmp).Hash.ToLower()
    if (-not $sums.ContainsKey($asset) -or $got -ne $sums[$asset]) {
        Remove-Item $tmp
        throw "$asset didn't match its checksum, so it wasn't installed. Try again."
    }
    $dest = Join-Path $dir $name
    if (Test-Path $dest) { Move-Item -Force $dest "$dest.old" }
    Move-Item -Force $tmp $dest
}

$path = [Environment]::GetEnvironmentVariable('Path', 'User')
if (-not ($path -split ';' | Where-Object { $_ -eq $dir })) {
    [Environment]::SetEnvironmentVariable('Path', ($(if ($path) { "$path;" } else { '' }) + $dir), 'User')
}

$shell = New-Object -ComObject WScript.Shell
$lnk = $shell.CreateShortcut((Join-Path ([Environment]::GetFolderPath('Programs')) 'MineNest.lnk'))
$lnk.TargetPath = Join-Path $dir 'minenest-gui.exe'
$lnk.WorkingDirectory = $dir
$lnk.Description = 'MineNest: easy Minecraft servers'
$lnk.Save()

Write-Host "MineNest is installed in $dir."
Write-Host 'Open it from the Start menu (MineNest), or type "minenest help" in a new terminal.'
Start-Process (Join-Path $dir 'minenest-gui.exe')
