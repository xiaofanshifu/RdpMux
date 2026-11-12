@echo off
setlocal
set "GOOS=windows"
set "GOARCH=amd64"
set "CGO_ENABLED=0"
cd /d "%~dp0"
go generate ./...
if errorlevel 1 goto :err
go build -trimpath -ldflags "-s -w" -o bin\rdpmux.exe .\cmd\rdpmux
if errorlevel 1 goto :err
go build -trimpath -ldflags "-s -w -H windowsgui" -o bin\rdpmuxgui.exe .\cmd\rdpmuxgui
if errorlevel 1 goto :err
powershell -NoProfile -Command "Compress-Archive -Path 'bin\rdpmux.exe','bin\rdpmuxgui.exe' -DestinationPath 'bin\RdpMux-win64.zip' -CompressionLevel Optimal -Force -ErrorAction Stop"
if errorlevel 1 goto :err
echo Build OK: bin\rdpmux.exe bin\rdpmuxgui.exe bin\RdpMux-win64.zip
exit /b 0
:err
echo Build failed.
exit /b 1
