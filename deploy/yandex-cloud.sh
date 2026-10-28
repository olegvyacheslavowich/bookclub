#!/usr/bin/env bash
# Deploys the book club Mini App to Yandex Cloud Serverless Containers.
# Safe to re-run: existing resources are reused, every run publishes a new revision.
#
# Needs: yc (after `yc init`, with a billing account linked), Docker (running), Go 1.24+, python3.
# Usage:
#   export BOT_TOKEN=123456:ABC...   # from @BotFather
#   export CLUB_CHAT_ID=-100...      # optional: only members of this group get in
#   ./deploy/yandex-cloud.sh
set -euo pipefail

APP=${APP:-bookclub}
CONTAINER=${CONTAINER:-bookclub}
SA_NAME=${SA_NAME:-bookclub-runner}
REGISTRY_NAME=${REGISTRY_NAME:-bookclub}
CONCURRENCY=${CONCURRENCY:-4}
CLUB_CHAT_ID=${CLUB_CHAT_ID:-}
: "${BOT_TOKEN:?Задайте BOT_TOKEN — токен бота от @BotFather}"

cd "$(dirname "$0")/.."

need() { command -v "$1" >/dev/null || { echo "Не найден $1. $2" >&2; exit 1; }; }
need yc "Установка: https://yandex.cloud/ru/docs/cli/quickstart"
need docker "Подойдёт Docker Desktop, OrbStack или Colima."
need go "Нужен Go 1.24+: https://go.dev/dl/"
need python3 "Он нужен скрипту для чтения JSON-ответов yc."

step() { printf '\n▶ %s\n' "$*"; }

# json_get FIELD: prints a top-level field of the JSON object on stdin (empty if absent).
json_get() { python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get(sys.argv[1], "") if isinstance(d, dict) else "")' "$1"; }

FOLDER_ID=$(yc config get folder-id 2>/dev/null || true)
[[ -n $FOLDER_ID ]] || { echo "Каталог не выбран: выполните yc init" >&2; exit 1; }
BUCKET=${BUCKET:-bookclub-$FOLDER_ID}
echo "Каталог: $FOLDER_ID"

step "Сервисный аккаунт $SA_NAME"
SA_ID=$(yc iam service-account get --name "$SA_NAME" --format json 2>/dev/null | json_get id 2>/dev/null || true)
if [[ -z $SA_ID ]]; then
  SA_ID=$(yc iam service-account create --name "$SA_NAME" --format json | json_get id)
fi
echo "  id: $SA_ID"
# Pull the image from Cloud Registry; read and write the data object (no deletes needed).
for role in cloud-registry.artifacts.puller storage.uploader; do
  if ! out=$(yc resource-manager folder add-access-binding "$FOLDER_ID" \
      --role "$role" --subject "serviceAccount:$SA_ID" 2>&1); then
    if [[ $out != *"already"* ]]; then
      echo "  ⚠ не удалось выдать роль $role: $out" >&2
    fi
  fi
done

step "Бакет $BUCKET"
if ! yc storage bucket get --name "$BUCKET" >/dev/null 2>&1; then
  yc storage bucket create --name "$BUCKET" >/dev/null
fi

step "Docker-реестр (Cloud Registry)"
REGISTRY_ID=$(yc cloud-registry registry list --folder-id "$FOLDER_ID" --format json 2>/dev/null | python3 -c '
import json, sys
regs = json.load(sys.stdin) or []
docker = [r for r in regs if str(r.get("kind", "")).lower() == "docker" and str(r.get("type", "")).lower() == "local"]
print(docker[0]["id"] if docker else "")' || true)
if [[ -z $REGISTRY_ID ]]; then
  REGISTRY_ID=$(yc cloud-registry registry create --folder-id "$FOLDER_ID" --name "$REGISTRY_NAME" \
    --registry-kind docker --registry-type local --format json | json_get id)
fi
echo "  id: $REGISTRY_ID"
yc cloud-registry configure-docker >/dev/null

TAG=$(date +%Y%m%d-%H%M%S)
IMAGE="registry.yandexcloud.net/$REGISTRY_ID/$APP:$TAG"

step "Сборка $IMAGE"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dist/bookclub ./cmd/bookclub
# Plain image manifest: Docker Desktop otherwise adds provenance/SBOM attestations,
# which turn the push into an image index the registry may reject.
docker build --platform linux/amd64 --provenance=false --sbom=false -t "$IMAGE" .
docker push "$IMAGE"

step "Контейнер $CONTAINER"
if ! yc serverless container get --name "$CONTAINER" >/dev/null 2>&1; then
  yc serverless container create --name "$CONTAINER" >/dev/null
fi

ENVIRONMENT="DATA_BUCKET=$BUCKET,BOT_TOKEN=$BOT_TOKEN"
[[ -n $CLUB_CHAT_ID ]] && ENVIRONMENT="$ENVIRONMENT,CLUB_CHAT_ID=$CLUB_CHAT_ID"

step "Новая ревизия"
yc serverless container revision deploy \
  --container-name "$CONTAINER" \
  --image "$IMAGE" \
  --cores 1 \
  --memory 128MB \
  --concurrency "$CONCURRENCY" \
  --execution-timeout 30s \
  --service-account-id "$SA_ID" \
  --environment "$ENVIRONMENT" >/dev/null

yc serverless container allow-unauthenticated-invoke "$CONTAINER" >/dev/null

URL=$(yc serverless container get --name "$CONTAINER" --format json | json_get url)
cat <<EOF

✅ Готово: $URL

Если это первый деплой:
  1. @BotFather → /newapp → выберите бота → URL: $URL
  2. Отправьте ссылку https://t.me/<имя_бота>/<короткое_имя> в группу клуба и закрепите.
Повторный запуск скрипта выкатывает новую версию по тому же адресу.
EOF
