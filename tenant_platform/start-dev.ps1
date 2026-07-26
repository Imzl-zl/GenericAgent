# 一键启动开发环境：先起 Postgres，再用两个新窗口分别起后端和前端
$root = $PSScriptRoot

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

# 启动后端
$backendScript = Join-Path $root "start-backend.ps1"
Write-Host "Starting backend in a new window..."
Start-Process powershell -ArgumentList "-NoExit", "-Command", "& '$backendScript'"

# 启动前端
$webScript = Join-Path $root "start-web.ps1"
Write-Host "Starting web frontend in a new window..."
Start-Process powershell -ArgumentList "-NoExit", "-Command", "& '$webScript'"

Write-Host ""
Write-Host "All services starting."
Write-Host "  Web:      http://localhost:5173"
Write-Host "  Backend:  http://127.0.0.1:8080"
Write-Host "  Admin:    通过部署者提供的独立后台入口"
