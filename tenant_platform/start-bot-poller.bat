@echo off
REM Bot Poller 启动脚本 (Windows)
REM 用途：启动 Python Bot Poller 服务，处理微信消息长轮询

cd /d "%~dp0"

echo 加载 .env 配置...
if not exist .env (
    echo 错误：找不到 .env 文件
    exit /b 1
)

REM 从 .env 文件读取配置
for /f "usebackq tokens=1,* delims==" %%a in (".env") do (
    set "line=%%a"
    if not "!line:~0,1!"=="#" (
        if not "%%a"=="" set "%%a=%%b"
    )
)

REM 检查必需的环境变量
if "%PLATFORM_WEBHOOK_SECRET%"=="" (
    echo ❌ 错误：PLATFORM_WEBHOOK_SECRET 未配置
    echo 请在 .env 文件中添加此配置
    exit /b 1
)

if "%BOT_POLLER_API_SECRET%"=="" (
    echo ❌ 错误：BOT_POLLER_API_SECRET 未配置
    echo 请在 .env 文件中添加此配置
    exit /b 1
)

REM 创建媒体目录
if "%MEDIA_DIR%"=="" set "MEDIA_DIR=.\runtime\media"
if not exist "%MEDIA_DIR%" mkdir "%MEDIA_DIR%"

REM 解析监听地址
if "%BOT_POLLER_LISTEN%"=="" set "BOT_POLLER_LISTEN=127.0.0.1:8081"

echo ==================================
echo 启动 Bot Poller
echo ==================================
echo 监听地址: %BOT_POLLER_LISTEN%
echo 媒体目录: %MEDIA_DIR%
echo Webhook URL: %PLATFORM_WEBHOOK_URL%
echo API 认证: 已启用
echo Webhook 认证: 已启用
echo ==================================
echo.

REM 启动 Bot Poller
python bot_poller\poller_server.py --listen %BOT_POLLER_LISTEN% --media-dir "%MEDIA_DIR%" --webhook-secret "%PLATFORM_WEBHOOK_SECRET%" --api-secret "%BOT_POLLER_API_SECRET%"
