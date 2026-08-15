# WhatsApp channel adapter — deferred, and exactly why

Phase 4 Task 5's brief covers both Signal and WhatsApp under the same
`ports/channels.Channel` port Task 4 established. Signal shipped
(`internal/adapters/signal`, `deploy/signal/README.md`). **WhatsApp did
not, and was never attempted as code** — no stub, no mock adapter, no
`internal/adapters/whatsapp` package. This document is the honest
substitute: exactly what the adapter would need, and why none of it
exists here yet.

## Why this is deferred, not just unfinished

Every other channel/side-effect adapter in this codebase (Slack, Signal,
MQTT, AnkiConnect, the weekly summary's SMTP) can be built and boot-time
validated with nothing more than config plumbing, because each either
dials *out* from this box (Slack's Socket Mode, Signal's sidecar) or
talks to something already running on the LAN. WhatsApp's Cloud API is
the one channel in the whole platform design that structurally cannot
work that way:

1. **A Meta Business account and an approved WhatsApp Business phone
   number.** Sending or receiving anything through the Cloud API
   requires a Meta Business Manager account, a WhatsApp Business
   Platform app registered under it, and a phone number verified and
   approved for that app — a multi-day approval process through Meta,
   not a config value. None of this exists for this project, and
   Task 5's instructions explicitly forbid obtaining it ("do not
   attempt to obtain credentials for either service, and do not sign up
   for anything").
2. **Public HTTPS ingress for inbound webhooks.** Unlike Signal (a
   sidecar this box dials out to) or Slack (Socket Mode, also outbound),
   WhatsApp delivers inbound messages by Meta's servers calling *this
   app's* webhook URL over the public internet, HTTPS only, with a
   Meta-signed payload. `deploy/compose.prod.yml`'s Caddy + Authelia
   stack fronts JLP for browser use on a LAN/home-server deployment; it
   is not, and per this project's PRD is deliberately not, exposed to
   the public internet. Standing up public ingress just to receive a
   webhook this repo has no approved number to test against would be
   scope creep against a security posture the rest of the project
   maintains on purpose.

Building a "WhatsApp adapter" against neither of these — a
webhook route with no Meta app to configure it against, a Cloud API
client with no access token that could ever authenticate — would not be
an adapter, it would be an elaborate stub wearing an adapter's shape:
code that has never received a byte from Meta and cannot be exercised
even manually, indistinguishable in the repo from working code until
someone tries to use it. That is exactly the failure mode Task 5's
honesty clause names and forbids.

## What the adapter would need, concretely

If a Meta Business account, an approved number, and public HTTPS
ingress all become available, here is the shape the work already fits:
`internal/adapters/whatsapp` implementing the same `ports/channels.
Channel` interface Slack and Signal already do (`Name`/`Start`/`Send`),
translating WhatsApp's own wire format into `channels.Inbound`/
`Outbound` and handing off to `application/channel.Service.Handle` —
zero new routing logic, exactly like the other two.

### 1. Inbound: a webhook route, not a poller

Unlike Slack (dials out) or Signal (dials a local sidecar), WhatsApp
inbound is Meta calling **this app**. That means, unlike every other
adapter in this codebase, `Channel.Start`'s "begins listening however
this channel does so" would mean *registering an HTTP route* on
`internal/adapters/http`'s existing router — the first channel adapter
that needs one — rather than opening an outbound connection of its own.
It would need to live **outside** the authenticated route group
(`internal/adapters/http/server.go`'s existing `Routes()`), since Meta's
servers carry no Authelia session or CSRF token; its own security is the
verify-token handshake and payload signature below, not JLP's cookie
auth.

```
GET  /webhooks/whatsapp   — verification handshake (below)
POST /webhooks/whatsapp   — inbound message delivery
```

### 2. The verify-token handshake (`GET`)

When you configure a webhook URL in the Meta App Dashboard, Meta issues
one `GET` request to confirm you control it:

```
GET /webhooks/whatsapp?hub.mode=subscribe
    &hub.verify_token=<APP_WHATSAPP_VERIFYTOKEN>
    &hub.challenge=<random-string>
```

The adapter must compare `hub.verify_token` against a config value set
ahead of time in the Meta dashboard (`APP_WHATSAPP_VERIFYTOKEN`, an
arbitrary shared secret this app picks, not something Meta issues) and,
if it matches, respond `200` with `hub.challenge`'s value as the raw
response body (not JSON — literally the challenge string). A mismatch
must return non-200 with no challenge echoed back, or anyone could
register their own webhook against a guessed/leaked token.

### 3. Inbound delivery (`POST`)

Every subsequent inbound message arrives as a `POST` whose body follows
Meta's webhook envelope. A single incoming text message looks like:

```json
{
  "object": "whatsapp_business_account",
  "entry": [{
    "id": "<WABA_ID>",
    "changes": [{
      "field": "messages",
      "value": {
        "messaging_product": "whatsapp",
        "metadata": { "display_phone_number": "15550001111", "phone_number_id": "<PHONE_NUMBER_ID>" },
        "contacts": [{ "profile": { "name": "Learner Name" }, "wa_id": "15555550100" }],
        "messages": [{
          "from": "15555550100",
          "id": "wamid.HBgL...",
          "timestamp": "1699999999",
          "type": "text",
          "text": { "body": "こんにちは" }
        }]
      }
    }]
  }]
}
```

Translation to `channels.Inbound` would be: `Channel: "whatsapp"`,
`ExternalID: messages[0].from` (the sender's WhatsApp ID, an
undelimited phone number — no `+`, unlike Signal's `Number`),
`Text: messages[0].text.body`, `ThreadID: messages[0].from` (WhatsApp
1:1 chats have no separate thread concept, same reasoning
`internal/adapters/signal` already documents for Signal's own
`ThreadID`). Every non-`text` `type` (image, audio, a delivery status
update under `changes[].value.statuses` rather than `.messages`, a
template-button reply) would need its own explicit drop-and-log path —
exactly like Slack's `SubType`/`BotID` filtering and Signal's
receipt-vs-dataMessage filtering already do for their own transports —
never silently forwarded as if it were a learner's text.

**Signature verification**: production Meta traffic signs every `POST`
body with `X-Hub-Signature-256`, an HMAC-SHA256 over the raw body keyed
by the Meta App Secret. The adapter must verify this signature and
reject (401, never process) any request that doesn't match — this is
the ONLY thing standing between "a request that looks like Meta" and
"an actual request from Meta," since the route sits outside JLP's own
authenticated group by necessity (see above).

### 4. Outbound: the Cloud API send call

Replying is a plain authenticated HTTP call to Meta's Graph API, not a
webhook:

```
POST https://graph.facebook.com/v22.0/<PHONE_NUMBER_ID>/messages
Authorization: Bearer <APP_WHATSAPP_ACCESSTOKEN>
Content-Type: application/json

{
  "messaging_product": "whatsapp",
  "to": "15555550100",
  "type": "text",
  "text": { "body": "..." }
}
```

`channels.Outbound{ThreadID, Text}` maps straightforwardly: `to:
out.ThreadID`, `text.body: out.Text`. `PHONE_NUMBER_ID` and
`APP_WHATSAPP_ACCESSTOKEN` are both issued by Meta once an app and
number are approved — the access token specifically is short-lived
unless exchanged for a long-lived System User token, which itself
requires the Business Manager setup step 1 above never got to.

**A wrinkle Slack/Signal don't have**: outside a 24-hour customer-
service window opened by the user's own last inbound message, Cloud API
sends must use a pre-approved *message template*, not free-form text —
Meta reviews and approves templates ahead of time. A real adapter would
need to track each conversation's 24-hour window (from the inbound
message's own `timestamp`) and fall back to a template send once it's
expired; this project's channel port doesn't currently model that
distinction at all (`Outbound` is free-form text only), which would
itself be a design question for whenever this actually gets built, not
something to guess at now.

### 5. Config shape (not implemented — for reference only)

Following this codebase's existing "empty ⇒ dormant, paired fields
validated together" pattern (`config.Slack`, `config.Signal`):

```go
type WhatsApp struct {
    VerifyToken   string // GET handshake shared secret (this app picks it)
    AccessToken   string // Cloud API bearer token (Meta issues it)
    PhoneNumberID string // Cloud API path segment (Meta issues it)
    AppSecret     string // X-Hub-Signature-256 HMAC key (Meta issues it)
}
```

All four empty would keep the route unregistered entirely (404, not
merely unauthenticated) — same "absent, not just guarded" contract
`config.A2A.Enabled`'s doc comment already establishes for A2A's own
routes.

## What would need re-validating before building this for real

- Whether Meta's webhook payload shape above (pulled from the
  documented Cloud API contract) still matches exactly at build time —
  this is a fast-moving API surface the Phase 4 roadmap already flagged
  for "ecosystem churn" re-validation.
- Whether `internal/adapters/http`'s router can cleanly host an
  unauthenticated route alongside the existing authenticated group
  without weakening anything else's posture — A2A (Task 3) mounts
  *inside* the authenticated group specifically to avoid this question;
  WhatsApp would be the first adapter that can't.
- The 24-hour-window / template-send distinction noted above, which has
  no home in `ports/channels` today.

None of this is a reason it can't be built — only a reason it wasn't
built *here*, against credentials nobody obtaining them, for a route
nobody can point real Meta traffic at to prove it works.
