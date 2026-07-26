param([string]$root)

# 兼容双击 / Start-Process / -Command 等启动方式
$ErrorActionPreference = 'Stop'
if (-not $root) {
    $root = if ($PSScriptRoot) { $PSScriptRoot } else { Split-Path -Parent $MyInvocation.MyCommand.Path }
}
if (-not $root) {
    throw "Unable to determine script directory. Please run this script from its own directory or pass -root."
}

# 加载 .env 到当前进程环境变量
$envFile = Join-Path $root ".env"
if (-not (Test-Path $envFile)) {
    throw "Missing .env file at $envFile"
}

Get-Content $envFile | ForEach-Object {
    if ($_ -match "^\s*#") { return }
    if ($_ -match "^\s*$") { return }
    $parts = $_ -split "=", 2
    if ($parts.Length -eq 2) {
        [Environment]::SetEnvironmentVariable($parts[0].Trim(), $parts[1].Trim(), "Process")
    }
}

Write-Host "Starting GenericAgent Platform web frontend..."
Write-Host "  URL: http://localhost:5173"

Set-Location (Join-Path $root "web")
npm run dev
