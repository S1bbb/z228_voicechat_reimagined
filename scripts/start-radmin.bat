@echo off
chcp 65001 >nul
rem Запуск для игры по Radmin VPN: друзьям — ссылка с IP 26.x.x.x.
cd /d "%~dp0.."
if not exist bin\z228.exe powershell -ExecutionPolicy Bypass -File scripts\build.ps1 || goto :err
bin\z228.exe -radmin -open %*
pause
goto :eof
:err
echo Сборка не удалась. Установлен ли Go?
pause
