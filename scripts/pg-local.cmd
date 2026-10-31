@echo off
rem Windows PowerShell 5.1 shortcut for scripts/pg-local.ps1.
rem   scripts\pg-local.cmd -Action start
rem   scripts\pg-local.cmd -Action status
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0pg-local.ps1" %*