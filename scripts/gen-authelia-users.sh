#!/usr/bin/env sh
# Generates deploy/authelia/users.dev.yml with a hashed dev password.
set -eu
PASSWORD="${AUTHELIA_DEV_PASSWORD:-devpassword}"
HASH=$(docker run --rm authelia/authelia:4 authelia crypto hash generate argon2 \
  --password "$PASSWORD" | sed 's/^Digest: //')
cat > deploy/authelia/users.dev.yml <<EOF
users:
  mikey:
    displayname: "Mikey Austin"
    password: "$HASH"
    email: mikey@jlp.localhost
    groups: [learners]
EOF
echo "wrote deploy/authelia/users.dev.yml"
