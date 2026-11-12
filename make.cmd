@REM Copyright (c) Microsoft Corporation.
@REM Licensed under the MIT License.

@echo off
setlocal
pwsh -NoProfile -ExecutionPolicy Bypass -File "%~dp0make.ps1" %*
exit /b %ERRORLEVEL%
