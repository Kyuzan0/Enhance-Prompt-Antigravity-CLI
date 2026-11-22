@echo off
echo Menghentikan Antigravity Prompt Enhancer...
powershell -Command "Get-CimInstance Win32_Process | Where-Object { $_.CommandLine -like '*daemon.ps1*' } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force }"
echo Selesai.
pause
