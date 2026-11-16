#!/usr/bin/env bash
# Выпуск новой версии на GitHub Releases. Нужны Go и GitHub CLI (gh auth login).
#   ./release.sh 1.1.0
set -euo pipefail
VER="${1:?Укажи версию, например: ./release.sh 1.1.0}"
TOPIC="$(tr -d '[:space:]' < topic.txt)"
[ -n "$TOPIC" ] || { echo "Нет topic.txt с топиком"; exit 1; }
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 \
  go build -trimpath -ldflags "-s -w -H windowsgui -X main.version=$VER -X main.defaultTopic=$TOPIC" -o KnopkaSbor.exe .
gh release create "v$VER" KnopkaSbor.exe --repo SerejaCrystalys/office_meetings --title "v$VER" --notes "${NOTES:-Версия $VER}"
echo "Готово: v$VER выпущена."
