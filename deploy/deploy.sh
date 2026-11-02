#!/bin/bash
set -euo pipefail

: "${FITBOT_IMAGE:?}" "${ENV_PARAM:?}" "${AWS_REGION:?}"
export FITBOT_IMAGE

cd /opt/fitbot
umask 077
aws ssm get-parameter --region "$AWS_REGION" --name "$ENV_PARAM" --with-decryption \
  --query Parameter.Value --output text > .env.tmp
mv .env.tmp .env

fail() {
  echo "$1" >&2
  docker compose logs --tail 100 app >&2
  exit 1
}

docker compose pull --quiet
# Caddy depends on a healthy app, so this fails if the app never gets there.
docker compose up -d --remove-orphans || fail "services did not start"

app=$(docker compose ps -q app)
for _ in $(seq 1 30); do
  status=$(docker inspect -f '{{.State.Health.Status}}' "$app")
  if [ "$status" = healthy ]; then
    # compose up leaves a running Caddy on its old config.
    docker compose exec -T caddy caddy reload --config /etc/caddy/Caddyfile
    echo "deployed $FITBOT_IMAGE"
    docker image prune -f > /dev/null
    exit 0
  fi
  sleep 2
done

fail "app did not become healthy (status: $status)"
