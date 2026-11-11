# CrowdVM installer for Windows, served by a CrowdVM server:
#   irm http://<server>:8420/install.ps1 | iex
#   $env:CROWDVM_NODE=1; irm http://<server>:8420/install.ps1 | iex   # CLI + node app
#   $env:CROWDVM_NO_SHELL=1 skips the PowerShell function; $env:CROWDVM_NO_LOGIN=1 skips sign-in.
$ErrorActionPreference = 'Stop'
$Server = if ($env:CROWDVM_SERVER) { $env:CROWDVM_SERVER } else { '__CROWDVM_SERVER__' }
$BinDir = if ($env:CROWDVM_BIN_DIR) { $env:CROWDVM_BIN_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\crowdvm' }
$Arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
New-Item -ItemType Directory -Force -Path $BinDir | Out-Null
function Fetch($name) {
  Invoke-WebRequest -UseBasicParsing "$Server/downloads/windows/$Arch/$name" -OutFile (Join-Path $BinDir $name)
}
Write-Host "Installing crowdvm from $Server into $BinDir"
Fetch 'crowdvm.exe'
if ($env:CROWDVM_NODE -eq '1') {
  Fetch 'crowdvm-node.exe'
  Fetch 'crowdvm-guest'
  Fetch 'crowdvm-linux-amd64'  # in-sandbox CLI for the config drive (--cli-binary default)
  if (-not (Get-Command qemu-system-x86_64.exe -ErrorAction SilentlyContinue)) {
    Write-Host 'note: install QEMU for Windows (https://qemu.weilnetz.de) and enable "Windows Hypervisor Platform" for fast VMs'
  }
  Write-Host "Next: crowdvm-node join --server $Server --token <join token>; crowdvm-node install-service"
}
$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if (-not ($userPath -split ';' | Where-Object { $_ -eq $BinDir })) {
  [Environment]::SetEnvironmentVariable('Path', "$userPath;$BinDir", 'User')
  Write-Host "Added $BinDir to your user PATH (restart the terminal)."
}
$Cli = Join-Path $BinDir 'crowdvm.exe'
& $Cli config set server $Server | Out-Null
if ($LASTEXITCODE -ne 0) { Write-Warning "could not save the server URL; run: crowdvm config set server $Server" }
# PowerShell function: gives each terminal its own "current" sandbox (crowdvm shell-init).
if ($env:CROWDVM_NO_SHELL -ne '1') {
  $ProfilePath = $PROFILE.CurrentUserAllHosts
  $Existing = if (Test-Path $ProfilePath) { Get-Content -Raw $ProfilePath } else { '' }
  if ($Existing -notmatch 'crowdvm.*shell-init') {
    New-Item -ItemType Directory -Force -Path (Split-Path $ProfilePath) | Out-Null
    $Q = $Cli -replace "'", "''"  # a path with an apostrophe (O'Brien) stays one quoted string
    $Line = "if (Test-Path '$Q') { & '$Q' shell-init powershell | Out-String | Invoke-Expression }"
    Add-Content -Path $ProfilePath -Value "`n# crowdvm: per-shell current sandbox`n$Line"
    Write-Host "Added the crowdvm function to $ProfilePath"
  }
}
if ($env:CROWDVM_NO_LOGIN -ne '1' -and [Environment]::UserInteractive -and -not [Console]::IsInputRedirected) {
  & $Cli onboard
  if ($LASTEXITCODE -ne 0) { Write-Host 'Sign in later with: crowdvm login' }
  Write-Host 'Done. Open a new terminal to use the crowdvm function.'
} else {
  Write-Host 'Done. Run: crowdvm onboard   (open a new terminal first to load the crowdvm function)'
}
