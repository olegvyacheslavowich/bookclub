#!/usr/bin/env bash
# Creates .env in the project root for deploy/yandex-cloud.sh: BOT_TOKEN, CLUB_CHAT_ID, MINIAPP_URL.
#
#   ./deploy/setup-env.sh
#
# BOT_TOKEN: from the environment, else from the running container in Yandex Cloud, else asked for.
# CLUB_CHAT_ID: found through getUpdates; write /start@<bot> in the club's group first.
# The token is only sent to api.telegram.org and written to .env (mode 600, git-ignored).
set -euo pipefail

cd "$(dirname "$0")/.."
CONTAINER=${CONTAINER:-bookclub}

need() { command -v "$1" >/dev/null || { echo "Не найден $1. $2" >&2; exit 1; }; }
need curl ""
need python3 "Он нужен для чтения JSON."

if [[ -f .env ]]; then
  read -r -p ".env уже есть. Перезаписать? [y/N] " ans
  [[ $ans == [yYдД]* ]] || { echo "Оставил как есть."; exit 0; }
fi

# ---- BOT_TOKEN ----
TOKEN=${BOT_TOKEN:-}
if [[ -z $TOKEN ]] && command -v yc >/dev/null; then
  # The latest revision of the container already has the token in its environment.
  TOKEN=$(yc serverless container revision list --container-name "$CONTAINER" --format json 2>/dev/null |
    python3 -c '
import json, sys
try:
    revs = json.load(sys.stdin)
except Exception:
    sys.exit(0)
revs = sorted(revs, key=lambda r: r.get("created_at", ""))
print((revs[-1].get("environment") or {}).get("BOT_TOKEN", "") if revs else "")
' || true)
  [[ -n $TOKEN ]] && echo "Токен бота взял из контейнера $CONTAINER в Yandex Cloud."
fi
if [[ -z $TOKEN ]]; then
  read -r -s -p "Токен бота от @BotFather: " TOKEN
  echo
fi
[[ $TOKEN == *:* ]] || { echo "Это не похоже на токен бота (нужен вид 123456:ABC...)." >&2; exit 1; }

api() { curl -sS --max-time 15 "https://api.telegram.org/bot$TOKEN/$1"; }

# ---- bot username ----
BOT=$(api getMe | python3 -c '
import json, sys
d = json.load(sys.stdin)
if not d.get("ok"):
    sys.exit("Telegram не принял токен: " + d.get("description", ""))
print(d["result"]["username"])
')
echo "Бот: @$BOT"

# ---- CLUB_CHAT_ID ----
find_groups() {
  api getUpdates | python3 -c '
import json, sys
d = json.load(sys.stdin)
if not d.get("ok"):
    sys.exit("getUpdates: " + d.get("description", ""))
groups = {}
def walk(v):
    if isinstance(v, dict):
        c = v.get("chat")
        if isinstance(c, dict) and c.get("type") in ("group", "supergroup"):
            groups[c["id"]] = c.get("title", "")
        for x in v.values():
            walk(x)
    elif isinstance(v, list):
        for x in v:
            walk(x)
walk(d.get("result", []))
for gid, title in groups.items():
    print(f"{gid}\t{title}")
'
}

FOUND=$(find_groups)
while [[ -z $FOUND ]]; do
  cat <<EOF

Не вижу ни одной группы. Сделайте так:
  1. Проверьте, что @$BOT добавлен в группу клуба.
  2. Напишите в группе: /start@$BOT
EOF
  read -r -p "Готово? Нажмите Enter (или Ctrl+C, чтобы выйти) " _
  FOUND=$(find_groups)
done

COUNT=$(printf '%s\n' "$FOUND" | wc -l | tr -d ' ')
if [[ $COUNT == 1 ]]; then
  CHAT_ID=$(cut -f1 <<<"$FOUND")
  echo "Группа: $(cut -f2 <<<"$FOUND") ($CHAT_ID)"
else
  echo "Бот видит несколько групп:"
  printf '%s\n' "$FOUND" | nl -w2 -s') '
  read -r -p "Номер группы клуба: " n
  CHAT_ID=$(printf '%s\n' "$FOUND" | sed -n "${n}p" | cut -f1)
  [[ -n $CHAT_ID ]] || { echo "Нет такого номера." >&2; exit 1; }
fi

# ---- MINIAPP_URL ----
read -r -p "Короткое имя Mini App из /newapp у @BotFather [club]: " SHORT
SHORT=${SHORT:-club}
APP_URL="https://t.me/$BOT/$SHORT"

umask 077
cat > .env <<EOF
BOT_TOKEN=$TOKEN
CLUB_CHAT_ID=$CHAT_ID
MINIAPP_URL=$APP_URL
EOF

cat <<EOF

✅ Записал .env:
   CLUB_CHAT_ID=$CHAT_ID
   MINIAPP_URL=$APP_URL
   BOT_TOKEN=(скрыт)

Проверьте, что $APP_URL открывает приложение, и выкатывайте:
   ./deploy/yandex-cloud.sh
EOF
