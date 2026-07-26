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

# 确保媒体文件目录存在
$mediaDir = Join-Path $root "runtime\bot_media"
New-Item -ItemType Directory -Force -Path $mediaDir | Out-Null

$pollerDir = Join-Path $root "bot_poller"

Write-Host "Starting Bot Poller..."
Write-Host "  Listen:    127.0.0.1:8081"
Write-Host "  Media Dir: $mediaDir"
Write-Host "  Webhook:   $env:PLATFORM_WEBHOOK_URL"
Write-Host ""
Write-Host "This service handles WeChat message polling and forwarding."
Write-Host "Press Ctrl+C to stop."
Write-Host ""

Set-Location $pollerDir

# 检查 Python 环境
$pythonCmd = "python"
try {
    $version = & $pythonCmd --version 2>&1
    Write-Host "Using Python: $version"
} catch {
    Write-Error "Python not found. Please install Python 3.10+ and ensure it's in your PATH."
    exit 1
}

# 启动 Bot Poller
& $pythonCmd poller_server.py `
    --listen=127.0.0.1:8081 `
    --api-secret=$env:BOT_POLLER_API_SECRET `
    --media-dir=$mediaDir
