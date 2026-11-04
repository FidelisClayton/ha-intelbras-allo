# Native LAN discovery

`internal/discovery.Discover(ctx, expectedUID) (netip.Addr, error)` searches only
for the selected cloud device UID. It sends the 18-byte ASCII query
`ASZENO.SEARCH.V4.1` immediately to `255.255.255.255:5000`, from local UDP
port 5003, then repeats once per second until a matching reply or the deadline.
It listens on local UDP ports 5001 and 5003. No authentication data is sent.
It does not enumerate devices, scan subnets, or log packets, UIDs or addresses.

The official SDK capture contained seven identical queries spaced 1.000, 1.003,
1.001, 1.000, 1.001 and 1.001 seconds apart. This supports repeating the search
query at 1 Hz; it does not authorize authentication or registration retries.

The complete operation, including waiting for another discovery call, has a
five-second maximum and honors earlier caller cancellation. Calls serialize
because the protocol uses fixed local ports. Socket binding does not enable
port reuse; an existing consumer produces `ErrPortInUse`. Malformed replies are
ignored while waiting for a matching reply. If the deadline expires after a
malformed reply, the result is `ErrDecode`; otherwise it is `ErrTimeout`.
Cancellation and cancellation while waiting for a slot preserve context errors.
Other socket failures produce `ErrNetwork`. Invalid selected UIDs produce
`ErrUID`. Errors contain no packet data or private identifiers.

## Reply framing

All integers below are unsigned little-endian. UDP payloads are limited to
65,507 bytes.

| Offset | Length | Meaning |
| --- | --- | --- |
| 0 | 16 | ASCII `ASZENO.SEARCH.V4` |
| 24 | 4 | Seed length, at most 32 |
| 28 | 4 | Ciphertext length, a multiple of 16 and at least 520 |
| 56 | seed length | Seed, zero-extended to 32 bytes for key derivation |
| 56 + seed length | ciphertext length | AES-256-CBC ciphertext |

The remaining header bytes are unproven and are not interpreted. The declared
lengths must cover the complete datagram. Decrypted bytes 200 through 263 are a
NUL-terminated ASCII alphanumeric UID, at most 63 characters. It must exactly
match `expectedUID`. Native code consumes the first 520 plaintext bytes; this
implementation does not require or remove PKCS#7 padding. Extra decrypted bytes
have no identity or addressing meaning.

Only a private, unicast IPv4 UDP source is accepted. The returned address comes
from the UDP source, never a payload IP field. The source UDP port is not fixed.
UID comparison filters discovery replies; it does not authenticate the LAN sender.

## Native key derivation

The protocol was traced through `GenerateSourceData` at native address
`0x479038` and `GenerateKey` at `0x477d24` through `0x477d48` in the official
SDK's `libqv-p2p-v2.so`. Its ELF load segment was checked before translating
virtual addresses into file offsets. Three native 4,096-byte tables begin at
`0x6e3157`, `0x6e4157`, and `0x6e5157`, respectively T1, T2 and T3.

The source includes only these 769 bytes of protocol lookup constants:

- `firstKey = T2[T1[1]]`.
- `nextKey[v] = T2[(v % 16) * v]`, for byte values v.
- `firstBox[v] = T3[v]`, for byte values v.
- `nextBox[v] = T3[(v % 16) * v]`, for byte values v.

Construct 32 key bytes by starting with `firstKey` and repeatedly looking up
`nextKey` using the previous byte. Add each zero-extended seed byte modulo 256.
Construct a 256-byte substitution box starting with `firstBox[key[0]]` and
repeatedly looking up `nextBox` using the previous box byte. Expand the key with
the AES-256 schedule using that substitution box. The actual AES key is expanded
bytes 8 through 39. Only the first 40 expanded bytes are needed, so the Go code
computes that prefix. Decryption uses standard AES-256-CBC with the 16 ASCII
characters `0000000000000000` as IV.

These constants describe the protocol algorithm. They are not an account key,
credential, native profile, extracted library or full table asset. No private
capture, device UID, address or credential is included in tests or documentation.

## Offline checks

The synthetic key answer and encrypted reply were generated independently with
Python and `cryptography`, using the native table algorithm. The synthetic
plaintext includes a selected test UID and deliberately non-PKCS#7 trailing
bytes. Tests inject packets at the transport boundary and open no UDP sockets.
A standard-library virtual-clock test drops the first query and answers the
second, verifying one-second spacing and immediate completion on the UID match.

Run with Go 1.26.3:

```sh
go test ./internal/discovery
go test -race ./internal/discovery
go vet ./internal/discovery
go build ./internal/discovery
```

Live broadcast reception and physical identity verification belong to the
parent's integration checks; offline tests do not establish hardware behavior.
