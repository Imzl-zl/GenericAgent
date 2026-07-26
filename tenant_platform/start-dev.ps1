# 一键启动开发环境：先起 Postgres，再用两个新窗口分别起后端和前端
$ErrorActionPreference = 'Stop'
$root = if ($PSScriptRoot) { $PSScriptRoot } else { Split-Path -Parent $MyInvocation.MyCommand.Path }
if (-not $root) {
    throw "Unable to determine script directory. Please run start-dev.ps1 from its own directory."
}

# 使用和当前终端一致的 PowerShell 可执行文件（PS7 / Windows PowerShell）
$shell = (Get-Process -Id $PID).Path
if (-not $shell -or -not (Test-Path $shell)) {
    $shell = 'powershell'
}

# 启动 Postgres
$postgresDir = Join-Path $root "infra\postgres"
if (Test-Path $postgresDir) {
    Write-Host "Starting Postgres..."
    Set-Location $postgresDir
    docker compose up -d
    Set-Location $root
}
else {
    Write-Warning "Postgres compose directory not found at $postgresDir"
}

# 启动 Bot Poller
$pollerScript = Join-Path $root "start-bot-poller.ps1"
Write-Host "Starting Bot Poller in a new window..."
Start-Process -FilePath $shell -ArgumentList "-NoExit", "-File", $pollerScript, $root

# 启动后端
$backendScript = Join-Path $root "start-backend.ps1"
Write-Host "Starting backend in a new window..."
Start-Process -FilePath $shell -ArgumentList "-NoExit", "-File", $backendScript, $root

# 启动前端
$webScript = Join-Path $root "start-web.ps1"
Write-Host "Starting web frontend in a new window..."
Start-Process -FilePath $shell -ArgumentList "-NoExit", "-File", $webScript, $root

Write-Host ""
Write-Host "All services starting."
Write-Host "  Web:        http://localhost:5173"
Write-Host "  Backend:    http://127.0.0.1:8080"
Write-Host "  Bot Poller: http://127.0.0.1:8081"
Write-Host "  Admin:      通过部署者提供的独立后台入口"
