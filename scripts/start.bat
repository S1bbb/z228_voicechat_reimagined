@echo off
chcp 65001 >nul
rem Обычный запуск: локалка + интернет (если проброшены порты).
cd /d "%~dp0.."
if not exist bin\z228.exe powershell -ExecutionPolicy Bypass -File scripts\build.ps1 || goto :err
bin\z228.exe -open %*
pause
goto :eof
:err
echo Сборка не удалась. Установлен ли Go?
pause
