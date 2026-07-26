@echo off
REM ===================================
REM 启动 GenericAgent Platform 后端
REM ===================================

cd /d "%~dp0"

REM 加载环境变量
for /f "usebackq tokens=1,* delims==" %%a in (".env") do (
    set "line=%%a"
    if not "!line:~0,1!"=="#" if not "%%a"=="" (
        set "%%a=%%b"
    )
)

REM 启动 Platform
backend-go\platform.exe ^
  --policy-file=contracts/policy/foundation.v1.json ^
  --claim-lease=10s ^
  --dev-loopback

pause
