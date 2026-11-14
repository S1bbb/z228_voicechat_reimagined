#!/usr/bin/env bash
# Ставит на VPS таймеры z228-health (проверка раз в минуту) и z228-nightly (перезапуск в 05:00 МСК).
#   Z228_VPS=user@host scripts/vps/install-timers.sh
# Создаёт только файлы z228-health.* / z228-nightly.* и /usr/local/sbin/z228-health; юнит z228 не меняет.
set -euo pipefail
: "${Z228_VPS:?Задайте Z228_VPS=user@host}"
cd "$(dirname "$0")"
SSH_OPTS=(-o BatchMode=yes -o ConnectTimeout=10)
scp "${SSH_OPTS[@]}" -q z228-health.sh "$Z228_VPS:/tmp/z228-health"
scp "${SSH_OPTS[@]}" -q z228-health.service z228-health.timer z228-nightly.service z228-nightly.timer "$Z228_VPS:/tmp/"
ssh "${SSH_OPTS[@]}" "$Z228_VPS" 'set -e
install -m 755 /tmp/z228-health /usr/local/sbin/z228-health
for f in z228-health.service z228-health.timer z228-nightly.service z228-nightly.timer; do install -m 644 /tmp/$f /etc/systemd/system/$f; rm -f /tmp/$f; done
rm -f /tmp/z228-health
systemctl daemon-reload
systemctl enable --now z228-health.timer z228-nightly.timer
systemctl list-timers --no-pager | grep z228'
