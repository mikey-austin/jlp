#!/usr/bin/env sh
# Generates deploy/authelia/users.dev.yml with a hashed dev password.
set -eu
PASSWORD="${AUTHELIA_DEV_PASSWORD:-devpassword}"
# Capture the docker invocation's own output (and let `set -e` abort on its
# exit status) before piping into sed — `HASH=$(docker ... | sed ...)` would
# only reflect sed's exit status under plain `set -eu` (no pipefail in
# POSIX sh), silently masking a failed/missing docker image pull.
OUT=$(docker run --rm authelia/authelia:4 authelia crypto hash generate argon2 --password "$PASSWORD")
HASH=$(echo "$OUT" | sed 's/^Digest: //')
case "$HASH" in
  '$argon2'*) ;;
  *)
    echo "gen-authelia-users.sh: unexpected hash output from authelia crypto hash generate: $OUT" >&2
    exit 1
    ;;
esac
cat > deploy/authelia/users.dev.yml <<EOF
users:
  mikey:
    displayname: "Mikey Austin"
    password: "$HASH"
    email: mikey@jlp.localhost
    groups: [learners]
EOF
echo "wrote deploy/authelia/users.dev.yml"
