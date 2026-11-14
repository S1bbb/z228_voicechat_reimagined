#!/usr/bin/env bash
# Проверка z228 раз в минуту (z228-health.timer). Три неудачи подряд — перезапуск z228.
# Трогает только сервис z228. Падения процесса ловит сам systemd (Restart=always), а это — на случай зависания.
set -u
STATE=/run/z228-health.fails
URL=http://127.0.0.1:3000/api/info

# сервис специально остановлен (деплой, ручная остановка) — не вмешиваемся
systemctl is-active --quiet z228 || { rm -f "$STATE"; exit 0; }

if curl -fsS -m 5 -o /dev/null "$URL"; then
  rm -f "$STATE"
  exit 0
fi
n=$(( $(cat "$STATE" 2>/dev/null || echo 0) + 1 ))
echo "$n" > "$STATE"
echo "z228 не отвечает ($n из 3)"
if [ "$n" -ge 3 ]; then
  echo "z228 завис — перезапускаю"
  rm -f "$STATE"
  systemctl restart z228
fi
