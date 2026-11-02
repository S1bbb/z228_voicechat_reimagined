# Сборка z228: bin\z228.exe (Windows) и bin/z228-linux (для VPS).
$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
  $env:Path = "C:\Program Files\Go\bin;$env:Path"
}
$ver = (git describe --tags --always --dirty 2>$null)
if (-not $ver) { $ver = "dev" }
$ld = "-s -w -X main.version=$ver"
New-Item -ItemType Directory -Force bin | Out-Null
Write-Host "Windows -> bin\z228.exe"
$env:CGO_ENABLED = "0"
go build -trimpath -ldflags $ld -o bin\z228.exe .\cmd\server
if ($args -contains "-linux") {
  Write-Host "Linux   -> bin\z228-linux"
  $env:GOOS = "linux"; $env:GOARCH = "amd64"
  go build -trimpath -ldflags $ld -o bin\z228-linux .\cmd\server
  Remove-Item Env:GOOS, Env:GOARCH
}
Write-Host "Готово."
