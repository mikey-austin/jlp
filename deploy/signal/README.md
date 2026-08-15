# Signal channel adapter — sidecar + device link

`internal/adapters/signal` (Phase 4 Task 5, PRD §20/§20.1) talks to a
[`signal-cli`](https://github.com/AsamK/signal-cli) sidecar running in
JSON-RPC daemon mode over a plain TCP socket
(`docker-compose.yml`'s `signal` profile — not started by a plain `make
up`). This document covers everything specific to that sidecar: getting
it running and linking it to a real Signal account. For how the adapter
itself works (routing, config, `APP_CHANNELS_ALLOWFROM`), see README.md's
own "Signal channel adapter" section.

## Honesty: what this repo can and can't do for you

- **Can**: build and unit-test the adapter entirely offline (no network,
  no signal-cli, no Signal account — see
  `internal/adapters/signal/signal_test.go`); start the sidecar and
  confirm its JSON-RPC endpoint is reachable and speaking the expected
  protocol (a `send`/`subscribeReceive` call against an unlinked account
  returns a `"not registered"` JSON-RPC error — that error IS the
  evidence the wiring is correct, not a failure).
- **Cannot**: complete registration or device-linking on your behalf.
  Signal's own security model requires scanning a QR code (or opening a
  `sgnl://linkdevice?...` URI) with the Signal app on a phone that
  already owns the phone number you want to link — there is no
  non-interactive, credential-free way to do this, by design. Nobody
  running this task had a phone to link, so **live Signal send/receive
  was never exercised** — only the sidecar's JSON-RPC surface (dry run
  below) and the adapter's own offline unit tests were.

## 1. Start the sidecar

```sh
make up-signal   # starts postgres + app + signal-cli — the app stays dormant (no Signal config set yet), only the sidecar is up
```

This starts `signal-cli` in multi-account daemon mode
(`daemon --tcp 0.0.0.0:6006`, **no** `-a <number>` of its own — see
`docker-compose.yml`'s comment on the service and
`internal/config.Signal`'s doc comment for why): every JSON-RPC call the
adapter makes names the linked number as the request's own `"account"`
param, rather than baking one number into the sidecar's startup command.
No host port is published — the sidecar is reachable only from other
containers on the compose network, same posture as `mosquitto`/`ollama`.

Its data — the linked device's private keys — lives in the `signaldata`
Docker volume, mounted at `/var/lib/signal-cli` inside the container.
**Back this volume up before destroying the host**; there is no way to
recover a linked device's keys otherwise, only to re-link from scratch.

## 2. Link a device (needs your phone)

```sh
make signal-register
```

This runs `signal-cli link -n "JLP"` inside the running sidecar
container, which prints a `sgnl://linkdevice?uuid=...&pub_key=...` URI
to the terminal. Turn that URI into a QR code (e.g.
`echo "$URI" | qrencode -t ansiutf8` on a machine with `qrencode`
installed, or any QR-code generator) and scan it from **Signal → Settings
→ Linked Devices → Link New Device** on the phone that owns the number
you want JLP to use. Once scanned, `signal-cli` completes the link and
the account becomes usable — no further command needed on this side.

This is the one step in this whole task nobody but the phone's owner can
do; everything else (the sidecar, the adapter, the config wiring) works
without it, just with no live account behind it yet.

## 3. Point the app at the linked number

Set in `.env`, then `make restart`:

```
APP_SIGNAL_RPCURL=signal-cli:6006
APP_SIGNAL_NUMBER=+15555550100      # the linked account's own E.164 number
APP_CHANNELS_ALLOWFROM=signal:+15555550100=dev,...   # sender's OWN number, once you know who's allowed to message the bot
```

`APP_SIGNAL_NUMBER` is the linked *bot* account; `APP_CHANNELS_ALLOWFROM`
entries name the *sender's* number that account is allowed to receive
messages from (PRD §20.1's untrusted-edge allow-list — see README.md).

## Dry-run evidence (no linked account)

With the sidecar started (`docker compose --profile signal up -d
signal-cli`) but no device ever linked, both a raw JSON-RPC call and the
real `internal/adapters/signal.Adapter` production code demonstrate the
wiring is correct even though nothing can actually be sent or received
yet:

```
$ docker compose --profile signal logs signal-cli
signal-cli-1  | INFO  DaemonCommand - Starting daemon in multi-account mode
signal-cli-1  | INFO  SocketHandler - Started JSON-RPC server on /[0:0:0:0:0:0:0:0]:6006

# raw TCP JSON-RPC round trip from another container on the compose network:
--> {"jsonrpc":"2.0","method":"send","params":{"account":"+15555550100","recipient":["+15555550199"],"message":"hello"},"id":1}
<-- {"jsonrpc":"2.0","error":{"code":-32602,"message":"Specified account does not exist","data":null},"id":1}

--> {"jsonrpc":"2.0","method":"subscribeReceive","params":{"account":"+15555550100"},"id":2}
<-- {"jsonrpc":"2.0","error":{"code":-32602,"message":"Specified account does not exist","data":null},"id":2}

# the real production adapter (signaladapter.New + Adapter.Start/Send) against the same sidecar:
2026/08/15 20:15:23 WARN signal: subscribeReceive failed, no inbound messages will be delivered until the account is linked err="signal: subscribeReceive: Specified account does not exist"
Adapter.Send() error: signal: send: Specified account does not exist
Adapter.Start() returned cleanly after ctx timeout
```

The daemon started fine (multi-account mode, no `-a`, exactly as
designed), accepted the TCP connection, decoded both JSON-RPC calls
correctly, and returned a well-formed JSON-RPC error naming the
unregistered account — this repo's own `"not registered"`-shaped
evidence the brief calls out as acceptable proof the wiring is right.
`internal/adapters/signal` logs this as a `WARN`, not fatal, and stays
up: `Adapter.Send()` surfaces the daemon's error as a plain Go error
rather than hanging or panicking, and `Adapter.Start()` returns cleanly
once its context is done — exactly like every other opt-in adapter in
this codebase degrades to "configured but not yet working" rather than
crashing. See
`.superpowers/sdd/2026-08-15-jlp-phase4/task-5-report.md` for the full
verification log this task recorded, including the app's clean boot
with no Signal config and the `docker compose up`-without-`--profile
signal` confirmation that the sidecar never starts uninvited.

## Not exercised by `go test`/`make test`/`make test-integration`

Same posture as Slack's own README section: the adapter's tests run
entirely against a fake in-process transport plus a real, but entirely
local, in-memory TCP listener standing in for signal-cli's own JSON-RPC
protocol (`internal/adapters/signal/signal_test.go`) — no real
signal-cli, no real Signal account, ever, in `go test`. Only the manual
steps above touch the real thing, and only as far as a phone-in-hand
device link, which this task could not itself perform.
