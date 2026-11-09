#!/usr/bin/env bash
# Деплой z228 на VPS: проверки → сборка → бэкап → замена бинаря → проверка → откат при сбое.
# Трогает ТОЛЬКО сервис z228. Подробности — docs/deploy-vps.md.
#
#   Z228_VPS=user@host scripts/deploy-vps.sh          # обычный деплой
#   Z228_VPS=user@host scripts/deploy-vps.sh --dry    # всё, кроме заливки на сервер
#
# Z228_VPS — ssh-адрес или алиас из ~/.ssh/config. Вход только по ключу (BatchMode: пароль не спросит).
set -euo pipefail

DRY=0
[ "${1:-}" = "--dry" ] && DRY=1
: "${Z228_VPS:?Задайте Z228_VPS=user@host (или алиас из ~/.ssh/config) — адрес сервера в репозиторий не пишем}"
SSH_OPTS=(-o BatchMode=yes -o ConnectTimeout=10)

cd "$(dirname "$0")/.."
say() { printf '\n== %s\n' "$*"; }

say "Проверка рабочей копии"
if [ -n "$(git status --porcelain -- cmd internal web go.mod go.sum)" ]; then
  echo "Есть незакоммиченные изменения в коде сервера/сайта — закоммитьте их (на сервере должна быть версия из git)."
  git status --short -- cmd internal web go.mod go.sum
  exit 1
fi
VER="$(git rev-parse --short HEAD)-vps"
echo "Версия: $VER ($(git log -1 --format=%s))"

say "Тесты"
go vet ./...
go test ./... >/dev/null && echo "go test: ok"
if command -v node >/dev/null; then
  for f in web/static/js/*.js; do node --check "$f"; done
  echo "node --check: ok"
fi

say "Сборка linux/amd64"
OUT_DIR="$(mktemp -d)"
trap 'rm -rf "$OUT_DIR"' EXIT
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$VER" -o "$OUT_DIR/z228-linux" ./cmd/server
echo "$(( $(wc -c < "$OUT_DIR/z228-linux") / 1024 / 1024 )) МБ"

if [ "$DRY" = 1 ]; then
  say "--dry: на сервер не заливаю"
  exit 0
fi

say "Заливка на сервер"
scp "${SSH_OPTS[@]}" -q "$OUT_DIR/z228-linux" "$Z228_VPS:/tmp/z228-new"

say "Замена бинаря и перезапуск z228"
# Внутри — только сервис z228 и его файлы. Ничего не печатаем из /etc/z228.env.
ssh "${SSH_OPTS[@]}" "$Z228_VPS" "VER='$VER' bash -s" <<'REMOTE'
set -euo pipefail
BIN=/usr/local/bin/z228
DATA=/var/lib/private/z228   # DynamicUser: настоящий путь к /var/lib/z228

chmod +x /tmp/z228-new                       # scp не сохраняет права
/tmp/z228-new -version | grep -q "$VER" || { echo "Залитый файл не той версии"; exit 1; }

BK=/root/z228-backup-$(date +%Y%m%d-%H%M%S)
mkdir -p "$BK"
cp -a "$BIN" "$BK/z228.prev"
cp -a "$DATA"/z228.db* "$BK/" 2>/dev/null || true
echo "Бэкап: $BK"

install -m 755 /tmp/z228-new "$BIN"
rm -f /tmp/z228-new
systemctl restart z228

ok=0
for i in 1 2 3 4 5 6 7 8 9 10; do
  sleep 1
  if systemctl is-active --quiet z228 && curl -fsS -m 3 http://127.0.0.1:3000/api/info >/dev/null 2>&1; then ok=1; break; fi
done
if [ "$ok" != 1 ]; then
  echo "!! z228 не поднялся — откатываю на прошлый бинарь"
  journalctl -u z228 -n 40 --no-pager | grep -v "TLS handshake" || true
  install -m 755 "$BK/z228.prev" "$BIN"
  systemctl restart z228
  sleep 2
  systemctl is-active z228 || true
  exit 1
fi
echo "Сервис: $(systemctl is-active z228), версия: $($BIN -version)"
REMOTE

say "Готово: на сервере $VER"
