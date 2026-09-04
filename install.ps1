<#
.SYNOPSIS
  Installs veans and marshal on Windows.

.DESCRIPTION
  A token is optional while the repository is public. Set GH_TOKEN or
  GITHUB_TOKEN, or be signed in to the gh CLI. A fine-grained token needs
  only "Contents: read" on this one repository.

  These are native windows/amd64 and windows/arm64 builds — the credential
  store is Windows Credential Manager, so no WSL is involved.

.EXAMPLE
  $env:GH_TOKEN = "ghp_xxx"
  iwr -useb https://raw.githubusercontent.com/EPYCD/veans/main/install.ps1 | iex

.PARAMETER Version
  Release tag to install. Defaults to the latest release.

.PARAMETER BinDir
  Install directory. Defaults to %LOCALAPPDATA%\Programs\veans.
#>
[CmdletBinding()]
param(
    [string]$Version,
    [string]$BinDir = (Join-Path $env:LOCALAPPDATA 'Programs\veans'),
    [string]$Repo = 'EPYCD/veans'
)

$ErrorActionPreference = 'Stop'
$api = "https://api.github.com/repos/$Repo"

function Info($m) { Write-Host "== $m" -ForegroundColor Cyan }
function Die($m) { Write-Host "error $m" -ForegroundColor Red; exit 1 }

# ------------------------------------------------------------- platform
$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { 'amd64' }
    'ARM64' { 'arm64' }
    default { Die "unsupported architecture: $env:PROCESSOR_ARCHITECTURE" }
}
Info "platform windows/$arch"

# ----------------------------------------------------------------- auth
$token = if ($env:GH_TOKEN) { $env:GH_TOKEN } elseif ($env:GITHUB_TOKEN) { $env:GITHUB_TOKEN } else { $null }
if (-not $token -and (Get-Command gh -ErrorAction SilentlyContinue)) {
    try { $token = (gh auth token 2>$null).Trim() } catch { }
}
# No token is not an error: the repository is public, so releases and their
# assets are readable anonymously - just rate-limited by IP. Only send
# Authorization when there is something to send; an empty Bearer is rejected.
$headers = @{
    'X-GitHub-Api-Version' = '2022-11-28'
    Accept                 = 'application/vnd.github+json'
}
if ($token) { $headers.Authorization = "Bearer $token" }

# -------------------------------------------------------------- resolve
try {
    $rel = if ($Version) {
        Invoke-RestMethod -Headers $headers -Uri "$api/releases/tags/$Version"
    } else {
        Invoke-RestMethod -Headers $headers -Uri "$api/releases/latest"
    }
} catch {
    Die "cannot read releases from $Repo - is the token authorised for it? $_"
}

$tag = $rel.tag_name
$assetName = "veans_${tag}_windows_${arch}.zip"
Info "release $tag"

$asset = $rel.assets | Where-Object { $_.name -eq $assetName } | Select-Object -First 1
if (-not $asset) { Die "release $tag has no asset named $assetName" }

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("veans-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp -Force | Out-Null
try {
    $zip = Join-Path $tmp $assetName

    # Assets download by id with an octet-stream Accept header. That path
    # works for public and private repositories alike.
    Info "downloading $assetName"
    $dl = @{ Accept = 'application/octet-stream' }
    if ($token) { $dl.Authorization = "Bearer $token" }
    Invoke-WebRequest -Headers $dl -Uri "$api/releases/assets/$($asset.id)" -OutFile $zip

    # ----------------------------------------------------------- checksum
    $sums = $rel.assets | Where-Object { $_.name -eq 'checksums.txt' } | Select-Object -First 1
    if ($sums) {
        $sumFile = Join-Path $tmp 'checksums.txt'
        Invoke-WebRequest -Headers $dl -Uri "$api/releases/assets/$($sums.id)" -OutFile $sumFile
        $line = Select-String -Path $sumFile -SimpleMatch " $assetName" | Select-Object -First 1
        if (-not $line) { Die "checksums.txt has no entry for $assetName" }
        $want = ($line.Line -split '\s+')[0]
        $got = (Get-FileHash -Algorithm SHA256 -Path $zip).Hash.ToLower()
        if ($want.ToLower() -ne $got) { Die "checksum mismatch for $assetName - refusing to install" }
        Info 'checksum verified'
    } else {
        Info 'no checksums.txt in the release; skipping verification'
    }

    # ------------------------------------------------------------ install
    Expand-Archive -Path $zip -DestinationPath $tmp -Force
    New-Item -ItemType Directory -Path $BinDir -Force | Out-Null
    foreach ($exe in 'veans.exe', 'marshal.exe') {
        Copy-Item -Path (Join-Path $tmp $exe) -Destination (Join-Path $BinDir $exe) -Force
        # Downloads carry a mark-of-the-web that blocks execution.
        Unblock-File -Path (Join-Path $BinDir $exe) -ErrorAction SilentlyContinue
    }
    Info "installed veans and marshal to $BinDir"

    # --------------------------------------------------------------- PATH
    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if (($userPath -split ';') -notcontains $BinDir) {
        [Environment]::SetEnvironmentVariable('Path', "$userPath;$BinDir", 'User')
        Info "added $BinDir to your user PATH - open a new terminal to pick it up"
    }
    $env:Path = "$env:Path;$BinDir"

    & (Join-Path $BinDir 'veans.exe') version
    & (Join-Path $BinDir 'marshal.exe') --version

    Write-Host ''
    Write-Host 'Next: from inside the repository you want coordinated, run'
    Write-Host ''
    Write-Host '  veans onboard --server https://your-board.example.com'
    Write-Host ''
    Write-Host 'That creates the project, the bots and every file the repo needs.'
} finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
