-- +goose Up
-- API tokens for non-browser clients (reader apps, the A2A chat server,
-- the Chrome extension). Under OIDC these clients cannot authenticate at
-- all: they have no browser to run a login flow in, so every request they
-- make gets a 401 or a redirect they cannot follow.
--
-- The shape deliberately mirrors APP_CHANNELS_ALLOWFROM (see
-- config.Channels.AllowFrom): this is an untrusted edge, so a token is
-- always bound to exactly one learner identity and grants exactly the
-- scopes it was minted with. Nothing is inferred from the request.
--
-- Only the HASH is stored. A token is shown to the operator once, at
-- creation, and is unrecoverable afterwards — a stolen database backup
-- must not yield working credentials. sha256 is the right primitive here
-- rather than a password KDF: these are 256-bit random secrets, not
-- human-chosen passwords, so there is no dictionary to attack and
-- nothing for bcrypt's work factor to buy.
CREATE TABLE api_tokens (
    id           uuid PRIMARY KEY,
    identity_id  text NOT NULL REFERENCES identities(id),
    -- Operator-facing label ("kobo reader", "a2a chat"), so a token can
    -- be revoked by knowing which app it belongs to rather than by
    -- recognising a hash.
    name         text NOT NULL,
    token_hash   text NOT NULL UNIQUE,
    -- Space-separated scopes, e.g. "vocabulary:write". Empty grants
    -- nothing: a token with no scope can authenticate but do nothing,
    -- which is the safe direction for a bug in the minting UI.
    scopes       text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    -- Set on every successful authentication, so an unused token is
    -- visible as unused and can be revoked with confidence.
    last_used_at timestamptz,
    -- Revocation is a soft delete, consistent with sessions/words
    -- (00026): the row stays so that "which app wrote this word" remains
    -- answerable after the token is gone.
    revoked_at   timestamptz
);

-- Authentication looks a token up by hash on every API request, and must
-- never match a revoked one.
CREATE INDEX api_tokens_live_hash_idx ON api_tokens (token_hash) WHERE revoked_at IS NULL;

-- The settings page lists a learner's own tokens, newest first.
CREATE INDEX api_tokens_identity_idx ON api_tokens (identity_id, created_at DESC);

-- +goose Down
DROP TABLE api_tokens;
