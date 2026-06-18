@echo off
setlocal
cd /d "%~dp0"
echo GenericAgent Desktop Windows setup
echo.
echo Recommended location: put this whole folder under GenericAgent\frontends\
echo Example: GenericAgent\frontends\GenericAgent-Desktop-Windows\install.bat
echo This script will prepare Python/.venv, install minimal dependencies, and write %%USERPROFILE%%\.ga_desktop_settings.json.
echo.
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0install_windows.ps1" -Mode PrepareOnly -SkipNpmInstall %*
if errorlevel 1 (
  echo.
  echo Setup failed. If the project root was not found, run:
  echo powershell -ExecutionPolicy Bypass -File .\install_windows.ps1 -ProjectDir D:\path\to\GenericAgent -Mode PrepareOnly -SkipNpmInstall
  echo.
  pause
  exit /b 1
)
echo.
echo Setup finished. You can now run start_windows.bat.
pause
