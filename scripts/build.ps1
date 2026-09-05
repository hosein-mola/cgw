$ErrorActionPreference = 'Stop'
$projectDir = Split-Path -Parent $PSScriptRoot
Push-Location $projectDir
$previousOS = $env:GOOS
$previousArch = $env:GOARCH
$previousCGO = $env:CGO_ENABLED
try {
    New-Item -ItemType Directory -Force -Path 'bin' | Out-Null
    $env:CGO_ENABLED = '0'
    foreach ($target in @('windows/amd64', 'linux/amd64', 'linux/arm64', 'darwin/amd64', 'darwin/arm64')) {
        $parts = $target.Split('/')
        $env:GOOS = $parts[0]
        $env:GOARCH = $parts[1]
        $name = "proxy-$($parts[0])-$($parts[1])"
        if ($parts[0] -eq 'windows') { $name += '.exe' }
        & go build -trimpath -o (Join-Path 'bin' $name) ./cmd/proxy
        if ($LASTEXITCODE -ne 0) { throw "Build failed for $target" }
        Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path 'bin' $name)
    }
} finally {
    $env:GOOS = $previousOS
    $env:GOARCH = $previousArch
    $env:CGO_ENABLED = $previousCGO
    Pop-Location
}
