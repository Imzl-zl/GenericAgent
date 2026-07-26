# 测试 mykey.py 生成功能
$ErrorActionPreference = 'Stop'

Write-Host "=== 测试 mykey.py 生成功能 ===" -ForegroundColor Cyan

# 1. 启动后端（后台运行）
Write-Host "`n1. 启动后端服务..." -ForegroundColor Yellow
$backendJob = Start-Job -ScriptBlock {
    Set-Location $using:PSScriptRoot
    .\start-backend.ps1
}

Start-Sleep -Seconds 5

# 2. 管理员登录获取 token
Write-Host "`n2. 管理员登录..." -ForegroundColor Yellow
$loginResp = Invoke-RestMethod -Uri "http://127.0.0.1:8080/v1/admin/login" `
    -Method POST `
    -Headers @{
        "Content-Type" = "application/json"
        "X-Platform-Dev-Token" = "f02adcb48a1eaa121a4e3b6edfdafdb30c860de02492d65de879ed0da9e74149"
    } `
    -Body '{"username":"admin","password":"admin"}' `
    -ErrorAction Stop

$token = $loginResp.token
Write-Host "✅ 登录成功，token: $($token.Substring(0,20))..." -ForegroundColor Green

# 3. 创建测试 Provider
Write-Host "`n3. 创建测试 LLM Provider..." -ForegroundColor Yellow
$providerData = @{
    name = "test-gpt"
    provider_type = "native_oai"
    base_url = "https://api.openai.com/v1"
    model = "gpt-4"
    api_key = "sk-test-key-12345"
    config = @{
        thinking_type = "adaptive"
        max_tokens = 8192
        temperature = 1.0
    }
} | ConvertTo-Json

$provider = Invoke-RestMethod -Uri "http://127.0.0.1:8080/v1/admin/llm-providers" `
    -Method POST `
    -Headers @{
        "Content-Type" = "application/json"
        "Authorization" = "Bearer $token"
    } `
    -Body $providerData `
    -ErrorAction Stop

Write-Host "✅ Provider 创建成功，ID: $($provider.id)" -ForegroundColor Green

# 4. 获取 mykey.py 内容
Write-Host "`n4. 获取生成的 mykey.py..." -ForegroundColor Yellow
$mykeyContent = Invoke-RestMethod -Uri "http://127.0.0.1:8080/v1/config/mykey.py" `
    -Method GET `
    -Headers @{
        "Authorization" = "Bearer $token"
    } `
    -ErrorAction Stop

Write-Host "✅ mykey.py 内容：" -ForegroundColor Green
Write-Host $mykeyContent -ForegroundColor White

# 5. 验证内容
Write-Host "`n5. 验证 mykey.py 内容..." -ForegroundColor Yellow
if ($mykeyContent -match "mixin_config") {
    Write-Host "  ✅ 包含 mixin_config" -ForegroundColor Green
} else {
    Write-Host "  ❌ 缺少 mixin_config" -ForegroundColor Red
}

if ($mykeyContent -match "test-gpt") {
    Write-Host "  ✅ 包含 provider name" -ForegroundColor Green
} else {
    Write-Host "  ❌ 缺少 provider name" -ForegroundColor Red
}

if ($mykeyContent -match "native_oai") {
    Write-Host "  ✅ 包含 provider type" -ForegroundColor Green
} else {
    Write-Host "  ❌ 缺少 provider type" -ForegroundColor Red
}

if ($mykeyContent -match "thinking_type") {
    Write-Host "  ✅ 包含 config 字段" -ForegroundColor Green
} else {
    Write-Host "  ❌ 缺少 config 字段" -ForegroundColor Red
}

# 6. 保存到文件
Write-Host "`n6. 保存到文件..." -ForegroundColor Yellow
$outputPath = Join-Path $PSScriptRoot "config\mykey.py"
$mykeyContent | Out-File -FilePath $outputPath -Encoding UTF8
Write-Host "✅ 已保存到: $outputPath" -ForegroundColor Green

# 7. 清理
Write-Host "`n7. 停止后端服务..." -ForegroundColor Yellow
Stop-Job -Job $backendJob
Remove-Job -Job $backendJob

Write-Host "`n=== 测试完成 ===" -ForegroundColor Cyan
