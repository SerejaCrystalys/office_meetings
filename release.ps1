# Выпуск новой версии на GitHub Releases (Windows). Нужны Go и GitHub CLI (gh auth login).
#   .\release.ps1 1.1.0
param([Parameter(Mandatory = $true)][string]$Version)
$ErrorActionPreference = "Stop"
$topic = (Get-Content topic.txt -Raw).Trim()
if (-not $topic) { throw "Нет topic.txt с топиком" }
$env:GOOS = "windows"; $env:GOARCH = "amd64"; $env:CGO_ENABLED = "0"
go build -trimpath -ldflags "-s -w -H windowsgui -X main.version=$Version -X main.defaultTopic=$topic" -o KnopkaSbor.exe .
if ($LASTEXITCODE -ne 0) { throw "Сборка не удалась" }
gh release create "v$Version" KnopkaSbor.exe --repo SerejaCrystalys/office_meetings --title "v$Version" --notes "Версия $Version"
if ($LASTEXITCODE -ne 0) { throw "Релиз не создан" }
Write-Host "Готово: v$Version выпущена."
