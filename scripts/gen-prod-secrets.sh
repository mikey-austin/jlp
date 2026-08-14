#!/usr/bin/env sh
# Generates deploy/authelia/secrets/{jwt_secret,session_secret,storage_encryption_key}
# (each openssl rand -hex 32) and deploy/authelia/users.prod.yml (a single
# hashed-password user for Authelia's file backend). Everything this writes
# is gitignored.
#
# Idempotent: existing secret files and an existing users.prod.yml are left
# untouched unless FORCE=1 is set — rerunning this script (e.g. as part of
# `make deploy-local`) never silently rotates credentials out from under a
# running stack.
#
# Reads AUTHELIA_PROD_USER/PASSWORD/EMAIL/DISPLAYNAME from the environment
# (or from deploy/.env.prod if it already exists, for reruns), falling back
# to defaults. Deliberately does NOT require deploy/.env.prod to exist yet —
# the documented verification flow runs this script first, then copies
# deploy/.env.prod.example to deploy/.env.prod.
set -eu

cd "$(dirname "$0")/.."

# Pick up values from an existing deploy/.env.prod without requiring one,
# so `FORCE=1 sh scripts/gen-prod-secrets.sh` after `make init`-style setup
# rotates using the same identity already on file.
if [ -f deploy/.env.prod ]; then
	set -a
	# shellcheck disable=SC1091
	. ./deploy/.env.prod
	set +a
fi

SECRETS_DIR=deploy/authelia/secrets
mkdir -p "$SECRETS_DIR"

gen_secret() {
	f="$SECRETS_DIR/$1"
	if [ -f "$f" ] && [ "${FORCE:-0}" != "1" ]; then
		echo "gen-prod-secrets.sh: $f already exists, leaving it (set FORCE=1 to regenerate)"
		return
	fi
	openssl rand -hex 32 >"$f"
	chmod 600 "$f"
	echo "gen-prod-secrets.sh: wrote $f"
}

gen_secret jwt_secret
gen_secret session_secret
gen_secret storage_encryption_key

if [ -f deploy/authelia/users.prod.yml ] && [ "${FORCE:-0}" != "1" ]; then
	echo "gen-prod-secrets.sh: deploy/authelia/users.prod.yml already exists, leaving it (set FORCE=1 to regenerate)"
	exit 0
fi

USER="${AUTHELIA_PROD_USER:-admin}"
EMAIL="${AUTHELIA_PROD_EMAIL:-admin@localhost}"
DISPLAYNAME="${AUTHELIA_PROD_DISPLAYNAME:-Admin}"
GENERATED=0
PASSWORD="${AUTHELIA_PROD_PASSWORD:-}"
if [ -z "$PASSWORD" ]; then
	PASSWORD=$(openssl rand -base64 18)
	GENERATED=1
fi

# Capture the docker invocation's own output (and let `set -e` abort on its
# exit status) before piping into sed — `HASH=$(docker ... | sed ...)` would
# only reflect sed's exit status under plain `set -eu` (no pipefail in
# POSIX sh), silently masking a failed/missing docker image pull.
OUT=$(docker run --rm authelia/authelia:4 authelia crypto hash generate argon2 --password "$PASSWORD")
HASH=$(echo "$OUT" | sed 's/^Digest: //')
case "$HASH" in
'$argon2'*) ;;
*)
	echo "gen-prod-secrets.sh: unexpected hash output from authelia crypto hash generate: $OUT" >&2
	exit 1
	;;
esac

cat >deploy/authelia/users.prod.yml <<EOF
users:
  $USER:
    displayname: "$DISPLAYNAME"
    password: "$HASH"
    email: $EMAIL
    groups: [learners]
EOF
echo "gen-prod-secrets.sh: wrote deploy/authelia/users.prod.yml (user: $USER)"

if [ "$GENERATED" = 1 ]; then
	echo "gen-prod-secrets.sh: AUTHELIA_PROD_PASSWORD was not set — generated a random login password for '$USER':"
	echo ""
	echo "    $PASSWORD"
	echo ""
	echo "Save it now: only the argon2 hash is persisted (in users.prod.yml), the plaintext is never written to disk."
fi
