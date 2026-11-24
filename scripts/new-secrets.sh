#!/bin/sh
# Creates secrets/<stage>/app.env and secrets/<stage>/migration.env with fresh random values.
#   sh scripts/new-secrets.sh <staging|production>
# Then paste the two Neon connection strings into them. Never overwrites existing files.
set -eu

stage=${1:-}
case "$stage" in
  staging | production) ;;
  *)
    echo 'Usage: sh scripts/new-secrets.sh <staging|production>' >&2
    exit 2
    ;;
esac

cd "$(dirname "$0")/.."
dir="secrets/$stage"

for f in app.env migration.env; do
  if [ -e "$dir/$f" ]; then
    echo "$dir/$f already exists; not overwriting" >&2
    exit 1
  fi
done

umask 077
mkdir -p "$dir"

cat >"$dir/app.env" <<EOF
# Secrets of the API (${stage}). Dotenv format; quote values that contain # or spaces.
# Neon POOLED connection string (host contains -pooler) of the restricted API role.
PG_URL=postgres://api_user:CHANGE_ME@ep-xxxx-pooler.region.aws.neon.tech/streetfood?sslmode=require
ADMIN_API_TOKEN=$(openssl rand -hex 32)
ENCRYPTION_KEY=$(openssl rand -base64 32)
EOF

cat >"$dir/migration.env" <<EOF
# Only the database, for the migration job (${stage}).
# Neon DIRECT connection string (no -pooler) of a role that may run DDL and create extensions.
PG_URL=postgres://owner:CHANGE_ME@ep-xxxx.region.aws.neon.tech/streetfood?sslmode=require
EOF

# The containers run as uid 1000. On Linux hand the files to it; elsewhere keep them private.
if [ "$(uname -s)" = Linux ] && [ "$(id -u)" = 0 ]; then
  chown 1000:1000 "$dir/app.env" "$dir/migration.env"
fi
chmod 400 "$dir/app.env" "$dir/migration.env"

echo "Created $dir/app.env and $dir/migration.env (mode 400)."
echo "Edit both to set PG_URL (chmod u+w first), then restore: chmod 400 $dir/*.env"
echo "Back up ENCRYPTION_KEY somewhere safe: without it stored merchant secrets cannot be read."
[ "$(uname -s)" != Linux ] || [ "$(id -u)" = 0 ] ||
  echo "On a Linux server run: sudo chown 1000:1000 $dir/*.env"
