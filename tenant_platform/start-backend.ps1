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

# 将相对路径解析为绝对路径
function Resolve-ProjectPath($path) {
    if ([System.IO.Path]::IsPathRooted($path)) { return $path }
    return Join-Path $root $path
}

$env:GA_RUNTIME_DIR = Resolve-ProjectPath $env:GA_RUNTIME_DIR
$env:GA_CONFIG_ROOT = Resolve-ProjectPath $env:GA_CONFIG_ROOT
$env:GA_LEGACY_ROOT = Resolve-ProjectPath $env:GA_LEGACY_ROOT
$env:GA_WORKER_SRC = Resolve-ProjectPath $env:GA_WORKER_SRC

# 确保 config 目录存在
New-Item -ItemType Directory -Force -Path $env:GA_CONFIG_ROOT | Out-Null

$policyFile = Join-Path $root "contracts\policy\foundation.v1.json"

Write-Host "Starting GenericAgent Platform backend..."
Write-Host "  Listen:    127.0.0.1:8080"
Write-Host "  Config:    $env:GA_CONFIG_ROOT"
Write-Host "  Runtime:   $env:GA_RUNTIME_DIR"
Write-Host "  Legacy:    $env:GA_LEGACY_ROOT"

Set-Location (Join-Path $root "backend-go")
go run ./cmd/platform/main.go `
  --dev-loopback `
  --policy-file=$policyFile `
  --claim-lease=30s `
  --listen=127.0.0.1:8080 `
  --database-url=$env:DATABASE_URL `
  --bot-token-key=$env:BOT_TOKEN_KEY `
  --ilink-base-url=$env:ILINK_BASE_URL `
  --ilink-app-id=$env:ILINK_APP_ID `
  --ilink-client-version=$env:ILINK_CLIENT_VERSION `
  --bot-poller-url=$env:BOT_POLLER_URL `
  --bot-poller-api-secret=$env:BOT_POLLER_API_SECRET `
  --platform-webhook-url=$env:PLATFORM_WEBHOOK_URL
