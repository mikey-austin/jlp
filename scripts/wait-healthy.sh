#!/usr/bin/env sh
# Polls a URL (curl -k, ignoring TLS trust — the prod stack uses Caddy's
# internal self-signed CA) until it responds successfully, or gives up.
# Usage: sh scripts/wait-healthy.sh <url>
set -eu
URL="${1:?usage: wait-healthy.sh <url>}"

i=0
while [ "$i" -lt 30 ]; do
	if curl -ksf -o /dev/null "$URL"; then
		echo "wait-healthy.sh: $URL is healthy"
		exit 0
	fi
	i=$((i + 1))
	sleep 2
done

echo "wait-healthy.sh: $URL did not become healthy after 30 attempts (60s)" >&2
echo "hint: check logs with 'make deploy-logs' (remote) or" >&2
echo "  'docker compose -f deploy/compose.prod.yml --env-file deploy/.env.prod logs'" >&2
exit 1
