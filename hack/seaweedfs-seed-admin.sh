#!/usr/bin/env sh
# Waits for the seaweedfs container (hack/docker-compose.emulators.yml) to accept
# connections, then seeds the one admin identity it boots with none of — without this,
# the S3/IAM service logs "loaded no identities - every request is denied" and refuses
# everything, including the IAM calls the seaweedfs-iam credential path's own tests
# need. Idempotent: `s3.configure -apply` on an existing identity just replaces it.
#
# Usage: hack/seaweedfs-seed-admin.sh <compose file>
set -eu
COMPOSE_FILE="${1:-hack/docker-compose.emulators.yml}"

echo "waiting for seaweedfs to accept connections on 127.0.0.1:19333..."
i=0
until curl -s -o /dev/null http://127.0.0.1:19333/ || [ "$i" -ge 60 ]; do
	i=$((i + 1))
	sleep 1
done
if [ "$i" -ge 60 ]; then
	echo "seaweedfs never came up" >&2
	exit 1
fi

echo "s3.configure -access_key booth-test -secret_key booth-test-secret-iam -user admin -actions Admin -apply" |
	docker compose -f "$COMPOSE_FILE" exec -T seaweedfs weed shell
