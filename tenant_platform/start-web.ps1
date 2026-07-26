# 加载 .env 到当前进程环境变量
$envFile = Join-Path $PSScriptRoot ".env"
if (-not (Test-Path $envFile)) {
    Write-Error "Missing .env file at $envFile"
    exit 1
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

Set-Location (Join-Path $PSScriptRoot "web")
npm run dev
