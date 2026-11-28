#!/bin/sh
# Runs docker compose for exactly one deployed stage, with its settings and Docker secrets.
#   sh scripts/compose.sh <staging|production> <compose arguments...>
#   sh scripts/compose.sh staging run --rm migrate          # apply pending migrations
#   sh scripts/compose.sh staging run --rm migrate version
#   sh scripts/compose.sh staging up -d app
#   sh scripts/compose.sh staging ps | logs -f app | down
#
# Needs .env.<stage> (non-secret settings: docker/env/<stage>.env.example) and the two secret
# files it names (scripts/new-secrets.sh <stage> creates them).
set -eu

stage=${1:-}
case "$stage" in
  staging | production) ;;
  *)
    echo 'Usage: sh scripts/compose.sh <staging|production> <compose arguments...>' >&2
    exit 2
    ;;
esac
shift

cd "$(dirname "$0")/.."

config="${COMPOSE_ENV_FILE:-.env.$stage}"
if [ ! -f "$config" ]; then
  echo "Missing $config: copy docker/env/$stage.env.example to it and fill it in" >&2
  exit 1
fi

# Reads NAME=value from the stage file (no shell evaluation of the file).
setting() { sed -n "s/^$1=//p" "$config" | tail -n 1 | sed "s/^['\"]//; s/['\"]\$//"; }

fail=0
for name in APP_SECRETS_PATH MIGRATION_SECRETS_PATH; do
  path=$(setting "$name")
  if [ -z "$path" ]; then
    echo "$config does not set $name" >&2
    fail=1
    continue
  fi
  if [ ! -f "$path" ]; then
    echo "$name: $path does not exist (sh scripts/new-secrets.sh $stage)" >&2
    fail=1
    continue
  fi
  # Compose bind-mounts the file with its host permissions, and the containers run as uid 1000.
  # Docker Desktop (macOS, Windows) does not enforce them, so check on Linux only.
  if [ "$(uname -s)" = Linux ]; then
    owner=$(stat -c %u "$path")
    if [ -n "$(find "$path" -perm -004)" ]; then
      echo "$path is readable by every user: chmod 400 $path" >&2
      fail=1
    elif [ "$owner" != 1000 ] && [ -z "$(find "$path" -perm -040)" ]; then
      echo "$path cannot be read by the container user: chown 1000:1000 $path && chmod 400 $path" >&2
      fail=1
    fi
  fi
done
[ "$fail" = 0 ] || exit 1

# An explicit stage wins over anything inherited from the shell.
export APP_ENV="$stage"

# Renamed from streetfood-<stage>. A stack still running under the old name holds the same
# port and the same Redis volume: stop it once, then everything runs under the new name.
if [ -n "$(docker compose --project-name "streetfood-$stage" ps --quiet 2>/dev/null)" ]; then
  echo "A stack from before the rename is still running as project streetfood-$stage." >&2
  echo "Stop it once, then run this again:" >&2
  echo "  docker compose --project-name streetfood-$stage down" >&2
  exit 1
fi

exec docker compose --project-name "goldenlane-$stage" --env-file "$config" \
  -f docker-compose.deploy.yml "$@"
