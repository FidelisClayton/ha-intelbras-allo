# Managed helper protocol

`cmd/allo-ha` reads private JSON lines on stdin and writes private JSON lines on
stdout. It opens no HTTP listener, reads no login credential file, and
accepts no credential arguments. The owner must keep both pipes private and must
not log their contents: device identifiers and WebRTC SDP are private data.

Each request has a positive integer `id` (signed 64-bit) and string `op`.
Each response contains that `id` and either an object `result` or
`error: {"code": "fixed_identifier"}`. Requests are processed serially. Errors
never include request text, credentials, SDP, cloud responses or native errors.
Malformed JSON, invalid IDs and oversized input use response ID `0`. A missing
operation with an otherwise valid ID retains that ID. Lines are limited to
2 MiB excluding the newline. An oversized line terminates the helper after the
safe error response; other malformed requests allow the next line to be read.

| Operation | Additional request fields | Result |
| --- | --- | --- |
| `login` | `email`, `password` (raw password) | `devices`: array of `{id,name,model}` |
| `select_device` | `device_id` | `credentials_ready: true` |
| `start_events` | `state_dir` (absolute private per-entry directory), optional `reauthenticate` boolean | `ready: true` |
| `resume_events` | `device_id`, `state_dir` (private adapter inputs) | `ready: true` |
| `prepare_media` | `email`, `password`, `device_id` (private adapter inputs) | `credentials_ready: true` |
| `open_media` | `offer` (complete SDP string) | `session_id`, `answer` (complete SDP string) |
| `close_media` | `session_id` | `closed: true` |
| `talk_start` | `session_id` | `talk_active: true` |
| `talk_stop` | `session_id` | `talk_active: false` |
| `talk_keepalive` | `session_id` | `talk_active: true` |

`login` authenticates once and lists devices. An explicit new login invalidates
previous account state and credentials. `select_device` obtains fresh opaque
credentials for an account device; these stay in Go memory. Selecting a device
invalidates previous device credentials even if the new selection fails.
`credentials_ready` does **not** mean media, bell push or integration availability
is ready. The login email and raw password stay only in Go memory for initial
push setup; a new login clears them before attempting authentication. There is
no automatic authentication or registration retry.

`start_events` requires selected device credentials. Home Assistant supplies its
private `.storage/intelbras_allo/<entry_id>` directory through the pipe, never
through argv or a frontend field. The push package persists each installation's
receiver identity and binding progress there with private directory/file modes
(0700/0600); it never persists the raw password. Existing receiver identities
are resumed. A fully bound receiver can listen after its saved OAuth token
expires. Expired OAuth with partial bindings returns `reauth_required` and
preserves the receiver state. Only an explicit private setup request with
`reauthenticate: true` renews OAuth once using the existing client UUID. The new
OAuth account must match the previously trusted account. Renewal saves new tokens
atomically while preserving the Google identity, FCM token, completed binding
flags and native alarm session; setup then completes only missing bindings.
Rejected authentication or a changed account leaves the saved state intact.

The response waits for all native bindings and an accepted Google MCS login,
with a 90-second initial deadline. Only then does the helper emit
`{"event":"availability","available":true}` and return `ready: true`. Startup
failure or timeout returns `events_failed` without exposing remote errors, except
for the fixed auth/state errors described here. A lost MCS transport or unexpected
clean listener exit emits availability false and retries in listen-only mode after
1, 2, 4 seconds, capped at 30 seconds. An accepted MCS login resets the delay and
emits availability true. Each retry uses the same device and private state directory,
with no email, password, OAuth renewal, Google registration or binding calls.
A non-listen failure stops retries. After initial readiness, terminal failures emit
`{"event":"push_error","code":"<fixed code>"}` followed by availability false;
codes are `reauth_required`, `resume_required` or `events_failed`.
EOF and cancellation stop listeners and pending retry timers.
A second active `start_events` or `resume_events`, login or device selection
returns `busy`. Media and talk can run concurrently with the event listener.

`resume_events` starts a saved receiver after helper recovery without login or
selection. It never creates an identity or performs setup. Missing, incomplete,
unsafe or mismatched saved receiver state returns `resume_required`; the adapter
must request explicit user repair rather than retrying setup. Its ready response
still requires an accepted MCS login. The adapter supplies `device_id` and
`state_dir` privately; neither is a frontend control.

`prepare_media` is allowed only for the selected active push receiver and while no
media view exists. A user opening the camera after helper recovery may explicitly
supply account credentials to obtain fresh native media credentials. Account
inventory and the returned device token must both match that receiver. Success
returns `credentials_ready: true`; it does not change the push identity or call
Google OAuth, registration or bindings. It is not a background authentication
operation. Login and device selection retain their existing busy guards.

Filtered physical bell events use
`{"event":"doorbell","device_id":"<selected device>","channel":1,"observed_at":"<UTC RFC3339 timestamp>"}`
with an optional `source_time` string from the source alarm. No raw alarm ID,
push payload or token is emitted. Events for another device or channel, and
events received before readiness, are ignored. Starting the listener produces
no bell event.

Each `open_media` freshly discovers the selected device on the LAN using its
cloud UID, with a five-second timeout, before opening native media. Only a
matching device's private IPv4 source address is used. Discovery failure returns
`discovery_failed` without opening a view. Device selection does not require LAN
discovery, so an offline device can still be selected. No host or IP field is
supported; unknown JSON fields, including `host`, are ignored and never used as
a media target.

Only one media view is supported. Login, device selection and opening another
view return `busy` while a view exists. Session IDs are random, per-view owner
tokens. Closing or changing talk requires the matching active session ID; stale
or foreign IDs return `invalid_session`. Starting talk twice returns `busy`.
Stopping already inactive talk on the current view succeeds. `talk_start` grants
a five-second transmission lease. While the microphone is actively held, the
card sends `talk_keepalive` every two seconds; each matching keepalive renews the
five-second lease without limiting continuous conversation length. A valid view
without active talk returns `talk_inactive`; an invalid view returns
`invalid_session`. Lease expiry closes native transmission and releases the talk
owner while keeping receive media alive. Stale expiry callbacks cannot stop a
renewed or later hold. Stop and close cancel the lease timer and talk context.

Media uses native channel 1, stream 2 and H264 plus PCMU WebRTC. The helper waits
for the peer to connect before forwarding native frames. Native receive buffering
is bounded by the media package. Native PCM16 mono 8 kHz is encoded to PCMU for
the browser. Browser microphone PCMU is continuously read and discarded unless
talk is active. Active talk decodes PCMU into PCM16; the native talk package owns
320-sample, 40 ms packet framing and pacing. No transcoding of video or external
STUN/TURN service is provided. This implementation is not evidence that live
voice or remote connectivity works on physical hardware.

A media or peer failure closes its view and can emit
`{"event":"media_closed","session_id":"<owner token>"}`. Events may interleave
with responses; each output line is serialized. Explicit close does not emit an
event. A talk-start failure returns `talk_failed`; a failure while forwarding an
active microphone closes the view. Native receive errors are observed by the
forwarding loop after the peer connects. Talk stop cancels native transmission
and discards pending partial audio. Closing the view also closes talk and WebRTC.

EOF and SIGTERM/context cancellation cancel in-flight operations, close native
sessions and the peer, and wait for helper media and push workers, including push state persistence. EOF can cancel an
unfinished request; keep stdin open until its response is received. Run owns and
closes its input reader. The executable exits nonzero on input/output failure
without printing raw errors.

Error codes: `invalid_request`, `unknown_op`, `not_authenticated`, `busy`,
`invalid_session`, `login_failed`, `device_list_failed`, `credentials_failed`,
`credentials_required`, `reauth_required`, `resume_required`, `events_failed`,
`discovery_failed`, `media_failed`, `invalid_offer`, `talk_failed`, `talk_inactive`.

Offline checks (Go 1.26.3):

```sh
go test ./internal/runtime ./cmd/allo-ha
go test -race ./internal/runtime ./cmd/allo-ha
go vet ./internal/runtime ./cmd/allo-ha
go build -o /tmp/allo-ha ./cmd/allo-ha
```

These checks do not validate cloud login, LAN discovery, physical media, physical
bell push, live browser/iPhone microphone transport or clean Home Assistant
installation. Those remain parent-owned release gates.
