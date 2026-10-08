#!/bin/sh
# Smoke-tests a built image: the bundled engine reports the version go.mod
# pins, and the service starts against a stand-in Bot API and answers its
# health endpoint. Usage: smoke.sh IMAGE [API_PORT] [HEALTH_PORT]
set -eu
image=$1
api_port=${2:-18081}
health_port=${3:-18080}
here=$(dirname "$0")

want=$(awk '$1 == "github.com/yaad-index/taste-machine" { print $2 }' "$here/../../go.mod")
got=$(docker run --rm --entrypoint taste-machine "$image" version)
if [ "$got" != "taste-machine $want" ]; then
  echo "bundled engine reports '$got', go.mod pins $want" >&2
  exit 1
fi
echo "engine: $got"

python3 "$here/fake_bot_api.py" "$api_port" &
api=$!
name="smoke-$$"
cleanup() {
  docker rm -f "$name" >/dev/null 2>&1 || true
  kill "$api" 2>/dev/null || true
}
trap cleanup EXIT

docker run -d --name "$name" --network host \
  -e TASTE_MACHINE_TELEGRAM_TOKEN=123:smoke \
  -e TASTE_MACHINE_TELEGRAM_ADMINS=1 \
  -e TASTE_MACHINE_TELEGRAM_API_URL="http://127.0.0.1:$api_port" \
  -e TASTE_MACHINE_TELEGRAM_HEALTH_ADDR="127.0.0.1:$health_port" \
  "$image" >/dev/null

for _ in $(seq 1 30); do
  if docker exec "$name" taste-machine-telegram health; then
    echo "health: ok"
    exit 0
  fi
  sleep 1
done
echo "the service never reported healthy; its log:" >&2
docker logs "$name" >&2
exit 1
