@echo off
setlocal
cd /d "%~dp0"
if not exist "hark2api.exe" (
  echo Building Hark2API...
  go build -o hark2api.exe .
  if errorlevel 1 (
    echo Build failed.
    pause
    exit /b 1
  )
)
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0start-hark2api.ps1"
if errorlevel 1 pause
