# Release a new version to GitHub Releases (Windows). Needs Go and GitHub CLI (gh auth login).
#   powershell -ExecutionPolicy Bypass -File .\release.ps1 1.3.0
param([Parameter(Mandatory = $true)][string]$Version)
$ErrorActionPreference = "Stop"
$topic = (Get-Content topic.txt -Raw).Trim()
if (-not $topic) { throw "topic.txt is empty or missing" }
$env:GOOS = "windows"; $env:GOARCH = "amd64"; $env:CGO_ENABLED = "0"
go build -trimpath -ldflags "-s -w -H windowsgui -X main.version=$Version -X main.defaultTopic=$topic" -o OfficeMeetings.exe .
if ($LASTEXITCODE -ne 0) { throw "Build failed" }
# Copy under the old name so clients older than 1.3.0 can still update
Copy-Item OfficeMeetings.exe KnopkaSbor.exe -Force
gh release create "v$Version" OfficeMeetings.exe KnopkaSbor.exe --repo SerejaCrystalys/office_meetings --title "v$Version" --notes "Version $Version"
if ($LASTEXITCODE -ne 0) { throw "Release failed" }
Remove-Item KnopkaSbor.exe
Write-Host "Done: v$Version released."
