# Release a new version to GitHub Releases (Windows). Needs Go and GitHub CLI (gh auth login).
#   powershell -ExecutionPolicy Bypass -File .\release.ps1 1.3.1
# The release gets exactly one asset: OfficeMeetings.exe
param([Parameter(Mandatory = $true)][string]$Version)
$ErrorActionPreference = "Stop"
$topic = (Get-Content topic.txt -Raw).Trim()
if (-not $topic) { throw "topic.txt is empty or missing" }
$env:GOOS = "windows"; $env:GOARCH = "amd64"; $env:CGO_ENABLED = "0"
go build -trimpath -ldflags "-s -w -H windowsgui -X main.version=$Version -X main.defaultTopic=$topic" -o OfficeMeetings.exe .
if ($LASTEXITCODE -ne 0) { throw "Build failed" }
gh release create "v$Version" OfficeMeetings.exe --repo SerejaCrystalys/office_meetings --title "v$Version" --notes "Version $Version"
if ($LASTEXITCODE -ne 0) { throw "Release failed" }
Write-Host "Done: v$Version released."
