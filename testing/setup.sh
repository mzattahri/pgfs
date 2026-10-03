#!/usr/bin/env sh

set -e

CONTAINER_IMAGE=${CONTAINER_IMAGE:-docker.io/library/postgres:$POSTGRES_VERSION-alpine}
CONTAINER_RUNTIME=${CONTAINER_RUNTIME:-$(command -v podman || command -v docker || true)}

if [ -z "$CONTAINER_RUNTIME" ]; then
  echo "Neither podman nor docker was found; set CONTAINER_RUNTIME." >&2
  exit 1
fi

echo Creating database container...
# Publish on a host port chosen by the runtime, so the
# test database never collides with one already in use.
CONTAINER_ID=`$CONTAINER_RUNTIME run --detach --env POSTGRES_DB=$POSTGRES_DB --env POSTGRES_USER=$POSTGRES_USER --env POSTGRES_PASSWORD=$POSTGRES_PASSWORD --publish 127.0.0.1::5432 $CONTAINER_IMAGE`

cleanup() {
  status=$?
  echo Removing database container $CONTAINER_ID...
  $CONTAINER_RUNTIME rm --force $CONTAINER_ID > /dev/null 2>&1 || true
  exit $status
}

trap cleanup EXIT

POSTGRES_PORT=`$CONTAINER_RUNTIME port $CONTAINER_ID 5432/tcp | head -n 1 | sed 's/.*://'`
POSTGRES_URL=postgres://$POSTGRES_USER:$POSTGRES_PASSWORD@127.0.0.1:$POSTGRES_PORT/$POSTGRES_DB
export POSTGRES_PORT POSTGRES_URL
echo Database listening on port $POSTGRES_PORT

# Run wrapped command
"$@"
