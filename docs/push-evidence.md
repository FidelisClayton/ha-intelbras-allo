# Direct Google push receiver

`internal/push.Run(ctx, Options{DeviceID, Email, Password, StateDir}, onEvent, onReady)` creates or resumes one installation's own receiver. It does not use MQTT, an emulator, Android account login, a private native-client profile, a client TLS certificate or a client private key. The caller supplies private inputs through the helper's private IPC channel; the library writes nothing to stdout or stderr.

`StateDir` must be an absolute path. The library creates missing directories with mode 0700 and rejects a nonprivate immediate parent or symlink ancestor. It keeps `push.json` and `push.lock` at mode 0600. JSON input is capped at 1 MiB; writes use a private temporary file, fsync, rename and directory fsync. A nonblocking exclusive lock prevents two receivers from modifying the same identity. State is associated with the selected device ID and is never imported from an existing phone, emulator or bridge.

A new random Firebase installation ID and client UUID are saved before the first network request. Setup has a one-minute deadline. The stages are:

1. Firebase installation, Google check-in and FCM sender token. Each completed phase is saved.
2. Intelbras OAuth login with this receiver's client UUID. The raw password is hashed for the protocol and never saved. OAuth tokens and expiry are private state.
3. Read the OAuth response's `cli-id` and numeric `qv-uid` claims. The client ID must equal `003-4077-<own UUID>`. The TLS-verified OAuth response is the authority for those claims; parsing a JWT is not independent signature verification.
4. Modern client-info registration, then FCM token binding. Each successful phase is saved and later explicit starts skip completed phases.
5. Native account login and UserAlarm `client-login` for the same UUID/token. The native account ID must equal the OAuth account ID. Only successful final registration saves the native registered flag.
6. Connect to Google MCS and invoke `onReady` only after an accepted LoginResponse. `Run` blocks until cancellation, stream failure or callback failure. Callback invocations are synchronous.

The fixed Intelbras profile is region `1`, language `pt`, system release `11` and the public app version. The timezone is the runtime's current local UTC offset, formatted `gmt±HH:MM`. Google's OS metadata remains the embedded API level `30`. `app.json` contains only shared app/project/package/version metadata; the selected device and account are runtime inputs.

A later `Run` reuses saved progress. It never automatically refreshes OAuth, retries authentication or loops over registration. An expired saved OAuth token returns `oauth_login:expired` without a cloud call when an Intelbras binding is still missing. Completed bindings continue to Google MCS with the saved Google identity even after OAuth expires, without requiring an account password. An explicit `Options.Reauthenticate` request renews OAuth once with the saved client UUID and requires the same previously trusted account. The candidate tokens are validated before replacing saved auth; successful renewal atomically preserves Google state, FCM token, completed binding flags and `AlarmSessionID`, then completes missing bindings. Login failure or account mismatch preserves the old file.

`Options.ListenOnly` requires a complete, safe saved receiver for the same device. It never generates a receiver identity or calls Google registration, OAuth login or native/modern binding. Missing or incomplete state fails before network activity. A stream failure returns `listen:failed`; the managed helper retries only listening with this option, no account credentials, and a 1, 2, 4 second backoff capped at 30 seconds. MCS acceptance resets the delay. Non-listen failures stop retries; cancellation also stops pending timers. The private `resume_events` operation uses this path after helper recovery. Interrupted or failed setup stages preserve all previously saved progress. A request accepted remotely just before a failed local save can need repetition on an explicit restart.

Errors exposed by `Run` have the exported type `*push.Error` with fixed `Stage` and `Code` fields. Stages include `input`, `configuration`, `trust`, `state`, `identity`, `google_registration`, `oauth_login`, `modern_binding`, `native_binding`, `bindings` and `listen`. Codes describe invalid input/state, missing credentials, expired authentication, timeout or stage failure; error text never contains remote bodies, URLs, credentials or device IDs. Cancellation returns the context error. `onReady` failure and event callback failure stop listening without exposing callback error text.

Events include schema/source/kind, selected device ID, one-based channel, opaque device source time and UTC observation time. They omit the raw alarm ID and raw payload. The receiver requires the configured package and Google sender, `AlarmEvent=19` and the selected `DevUmid`; incompatible payload forms are ignored. The last 1024 Google persistent IDs suppress replay. Delivery is emit → persist replay ID → ACK. An event callback or state-save failure prevents ACK; a crash between steps can duplicate delivery. This is at-least-once transport handling, not proof of one event per physical press.

Google uses system CA/DNS validation and fixed Google hosts. Intelbras OAuth/OpenAPI use system CA/DNS verification plus leaf DER SHA-256 `f35f1c7d67e7c32e439e11e15a189ffda7664b3d30747a51a795b279dd742f87`. Native account/UserAlarm use the approved public CA supplied by `bootstrap.VendorCA()`, server-auth chain verification and exact leaf DER SHA-256 `e7167be07fc554614ce611b1be054c29cfc39926695d6a6717c9e631dc83276a`. Their fixed hosts are `intelbras-4.qvcloud.net:443` and `intelbras-1.qvcloud.net:4443`. The native certificate lacks DNS names, so its manual verifier validates the approved chain, server-auth usage, exact pin and destination/SNI allowlist. Redirects are rejected. No endpoint, pin or insecure mode is configurable.

## Offline validation

The synthetic protocol tests were copied before extracting their implementation and initially failed because the implementation/dependencies were absent. The completed tests cover Google registration/resume and HTTP bounds; MCS fragmentation, login rejection/readiness, heartbeat, cancellation, package/sender/device/event filters, replay IDs, emit/save-before-ACK; native request layouts, account identity and response validation; TLS chain/pin/redirect restrictions without a client certificate; private file permissions/symlinks/atomic save; JWT account identity; new-install identity persistence, stage failure/resume, expired OAuth rejection for pending bindings, continued listening after OAuth expiry for completed bindings, no repeated bindings on listener restart; explicit renewal with retained identity/binding progress, rejection of changed accounts and failed logins without modifying saved state, and listen-only rejection of missing/incomplete/mismatched receiver state. Runtime tests cover bounded backoff and reset, private resume inputs, cancellation during retry, terminal fixed errors, native PCM-to-PCMU and H264 forwarding, held-only microphone routing, stop during a blocked write, a second hold, lease renewal and automatic lease expiry without a stop request. Transports are fake HTTP or in-memory `net.Pipe`; they do not contact external services.

Run from the repository root:

```sh
mise exec go@1.26.3 -- go test ./internal/push/...
mise exec go@1.26.3 -- go test -race ./internal/push/...
mise exec go@1.26.3 -- go vet ./internal/push/...
mise exec go@1.26.3 -- go build ./internal/push/...
mise exec go@1.26.3 -- go mod verify
```

## Physical evidence and remaining gates

The source standalone receiver observed one owner-confirmed physical bell with the emulator shut down at `2026-11-04T02:24:43.509917091Z`; the source timestamp was `2026-11-03 23:24:41` in America/Sao_Paulo. This is evidence for the extracted wire sequence, not a live test of this public installation.

A fresh public installation's Firebase/check-in/FCM registration, OAuth claim identity, modern/native bindings, physical push reception, helper-to-Home-Assistant delivery, sustained reconnect behavior, per-press correlation and clean supported Home Assistant installation remain unverified. No worker live requests were made. Do not describe the integration as installable or release-ready from offline tests alone.
