#!/usr/bin/env sh
# Prints the environment variables that point the test suite at the containers from
# docker-compose.emulators.yml. Usage: eval "$(hack/test-env.sh)"
cat <<'VARS'
export BOOTH_TEST_S3_ENDPOINT=http://127.0.0.1:19000
export BOOTH_TEST_S3_ACCESS_KEY=booth-test
export BOOTH_TEST_S3_SECRET_KEY=booth-test-secret
export BOOTH_TEST_AZURITE_ENDPOINT=http://127.0.0.1:10000/devstoreaccount1
export BOOTH_TEST_POSTGRES_DSN=postgres://booth:booth-test@127.0.0.1:15432/booth_storage_test?sslmode=disable
VARS
