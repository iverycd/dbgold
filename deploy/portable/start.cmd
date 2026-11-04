@echo off
setlocal
set "TEMP=%~dp0tmp"
set "TMP=%TEMP%"
set "SQLITE_TMPDIR=%TEMP%"
"%~dp0dbgold.exe" portable %*
set "result=%errorlevel%"
if not "%result%" == "0" pause
exit /b %result%
