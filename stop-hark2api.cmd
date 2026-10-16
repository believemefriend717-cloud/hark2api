@echo off
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0stop-hark2api.ps1"
if errorlevel 1 pause
