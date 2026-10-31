@echo off
rem Windows PowerShell 5.1 shortcut for scripts/verify.ps1.
rem pwsh (PowerShell 7) is NOT installed on every machine; this wrapper keeps the
rem command short and the execution policy out of the way.
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0verify.ps1" %*