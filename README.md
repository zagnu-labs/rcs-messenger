# rcs-messenger

Sends RCS messages from an Android phone through a paired Google Messages for web session, using [libgm](https://github.com/mautrix/gmessages) from mautrix-gmessages. It refuses to send rather than fall back to SMS.

## Run

```
docker compose up -d
go run ./cmd/server
```

The server listens on `127.0.0.1:8080` and creates its two tables on startup.

| Variable | Default | Meaning |
|---|---|---|
| `ADDR` | `127.0.0.1:8080` | Listen address. The API has no auth, so keep it on localhost. |
| `DATABASE_URL` | `postgres://postgres:postgres@localhost:5432/rcs_messenger?sslmode=disable` | Postgres holding the paired session and the send results |
| `RCS_ENABLED` | `true` | Run the phone connection and allow sends |
| `RCS_DRY_RUN` | `false` | Accept sends without reaching the phone |
| `RCS_WAIT_SECONDS` | `20` | How long a send waits for a delivery status, capped at 20 |
| `RCS_MONITOR_SECONDS` | `60` | How often the health monitor checks the phone |
| `RCS_ALLOWLIST_PHONES` | empty | Comma-separated E.164 numbers; when set, only these can be sent to |
| `DEBUG` | `false` | Debug logging |

## The send rule

A message is sent only when the phone's own records confirm it will be end-to-end encrypted. There is no setting that relaxes this, and `RCS_REQUIRE_E2EE=false` stops the server from starting.

| What the phone says | Result |
|---|---|
| Chat has an encrypted protocol tombstone | Send |
| Chat is an unfinalized first draft and the capability record reads `yes` | Send |
| First draft, capability `not_yet` or the probe failed | Refuse as `e2ee_pending`, retryable. An iPhone on plain RCS stays here forever, so it is never sent to |
| First draft, capability `no` | Refuse as `not_e2ee` |
| Chat recorded as RCS without encryption | Refuse as `not_e2ee` |
| No protocol tombstone in reach | Refuse as `not_e2ee` |
| Chat is SMS, SMS/MMS send mode, RCS off on the SIM, read-only, phone offline | Refuse |

After a send the chat is re-read as a second check. If the message went out unencrypted or as SMS anyway, it is logged at error level, recorded, and that number is never sent to again.

## Android transport (no Google web session)

`RCS_TRANSPORT=android` sends by driving the Google Messages app on an Android phone over adb and reads the encrypted/plain/SMS answer off the screen before sending. Nothing expires, nothing is paired. Setup, settings, what is verified and what still needs a real RCS phone: [docs/ANDROID.md](docs/ANDROID.md). Try it without the server: `go run ./cmd/androidprobe check +15551234567`.

## Pair a phone (web transport)

Open http://localhost:8080/internal/rcs/pair.

- **QR**: on the phone, Google Messages > profile menu > Device pairing > QR code scanner. Google has removed QR pairing in some countries.
- **Emoji**: POST a HAR captured on messages.google.com/web, signed in to the phone's Google account, to `/internal/rcs/pair/cookies`, then tap the emoji it returns on the phone.

The Google session behind emoji pairing expires within hours to a day. `/internal/rcs/pair/refresh` takes a fresh HAR without pairing again. Keeping that session alive is the open problem.

## Send

```
curl -X POST localhost:8080/internal/rcs/send \
  -H 'Content-Type: application/json' \
  -d '{"to":"+15551234567","message":"hi"}'
```

Every endpoint is documented in `bruno/`, including what each refusal reason means.

## Code

- `cmd/server`: config, wiring and routes
- `internal/providers/rcsphone`: the phone session, pairing, eligibility checks, sending, the contact capability lookup
- `internal/providers/rcsdirect`: the request and result types shared by the layers
- `internal/rcs`: the send policy, per-number send results, opt-out replies and the health monitor
- `internal/api/rcs`: HTTP handlers and the pairing page
- `internal/db`: Postgres connection and schema

```
go test ./...
```

The opt-out and results store tests need a real Postgres and are skipped without one:

```
docker run -d --rm --name rcs-test-pg -e POSTGRES_PASSWORD=pg -e POSTGRES_DB=rcs_test -p 127.0.0.1:55432:5432 postgres:16
RCS_TEST_DATABASE_URL='postgres://postgres:pg@127.0.0.1:55432/rcs_test?sslmode=disable' go test ./internal/rcs
```
