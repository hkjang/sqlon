param(
  [string]$OutputDir = "dist"
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
$out = Join-Path $root $OutputDir
New-Item -ItemType Directory -Force -Path $out | Out-Null

$targets = @(
  @{ GOOS = "windows"; GOARCH = "amd64"; Name = "jamypg-mcp-windows-amd64.exe" },
  @{ GOOS = "linux"; GOARCH = "amd64"; Name = "jamypg-mcp-linux-amd64" },
  @{ GOOS = "linux"; GOARCH = "arm64"; Name = "jamypg-mcp-linux-arm64" }
)

foreach ($target in $targets) {
  $env:GOOS = $target.GOOS
  $env:GOARCH = $target.GOARCH
  $env:CGO_ENABLED = "0"
  $dest = Join-Path $out $target.Name
  go build -trimpath -ldflags="-s -w" -o $dest ./cmd/jamypg-mcp
  Write-Host "built $dest"
}

Remove-Item Env:\GOOS -ErrorAction SilentlyContinue
Remove-Item Env:\GOARCH -ErrorAction SilentlyContinue
Remove-Item Env:\CGO_ENABLED -ErrorAction SilentlyContinue
