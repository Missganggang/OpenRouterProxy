param([string]$Version = (Get-Date -Format 'yyyyMMddHHmmss'))
$ErrorActionPreference = 'Stop'
if ($Version -notmatch '^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$') {
    throw 'Version must use 1-32 letters, digits, dots, underscores or hyphens'
}
$projectRoot = Split-Path -Parent $PSScriptRoot
$previousEnvironment = @{}
foreach ($name in @('GOOS', 'GOARCH', 'GOAMD64', 'CGO_ENABLED')) {
    $previousEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
}
Push-Location (Join-Path $projectRoot 'openroute')
try {
    $env:GOOS = 'linux'
    $env:GOARCH = 'amd64'
    $env:GOAMD64 = 'v1'
    $env:CGO_ENABLED = '0'
    & go build -trimpath -ldflags "-s -w -X github.com/openroute/openroute/internal/app.BuildStamp=$Version" -o (Join-Path $PSScriptRoot 'openroute') .
    if ($LASTEXITCODE -ne 0) { throw 'Panel build failed' }
    foreach ($arch in @('amd64', 'amd64v3', 'arm64')) {
        $env:GOARCH = if ($arch -eq 'arm64') { 'arm64' } else { 'amd64' }
        $env:GOAMD64 = if ($arch -eq 'amd64v3') { 'v3' } else { 'v1' }
        $destination = Join-Path $PSScriptRoot "node-binaries/$arch"
        New-Item -ItemType Directory -Force $destination | Out-Null
        & go build -trimpath -ldflags "-s -w -X main.version=nc$Version" -o (Join-Path $destination 'rel_nodeclient') ./cmd/nodeclient
        if ($LASTEXITCODE -ne 0) { throw "Node build failed: $arch" }
		[System.IO.File]::WriteAllText((Join-Path $destination 'version.txt'), "nc$Version", [System.Text.UTF8Encoding]::new($false))
    }
    Write-Host "Built panel and Linux node clients: $Version"
} finally {
    Pop-Location
    foreach ($name in $previousEnvironment.Keys) {
        [Environment]::SetEnvironmentVariable($name, $previousEnvironment[$name], 'Process')
    }
}
