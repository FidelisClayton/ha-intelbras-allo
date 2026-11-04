# Synthetic WebRTC transport evidence

**Historical milestone:** this document records the synthetic transport milestone on 2026-11-04. Statements below about missing integration, packaging, UI and native bridging describe that milestone, not the current implementation. See the [installation gates](installation.md#gates-before-the-first-release) for current candidate validation, owner-reported physical confirmation and remaining release requirements.

This repository contains an encoded-media transport seam, not an installable Home Assistant integration. These results use two in-process Go peers and synthetic media on Linux amd64. No device, browser, account service, private configuration or production bridge was accessed.

## Dependency and media boundary

- **WebRTC:** `github.com/pion/webrtc/v4 v4.2.26`, pinned in `go.mod` and `go.sum`. The [official release](https://github.com/pion/webrtc/releases/tag/v4.2.26) and GitHub releases API identified this stable version on 2026-11-04. It implements ICE, DTLS, SRTP, negotiation and RTP packetization; this package adds no custom ICE implementation.
- **License:** Pion WebRTC and the direct RTP dependency (`github.com/pion/rtp v1.10.6`) use MIT. See the [WebRTC license](https://github.com/pion/webrtc/blob/v4.2.26/LICENSE) and [RTP license](https://github.com/pion/rtp/blob/v1.10.6/LICENSE). No upstream source was copied. A future binary/release must carry dependency notices, including transitive dependencies; release packaging and that notice audit remain unimplemented.
- **Video:** `WriteVideo` accepts one complete H264 Annex B access unit with its duration. Pion fragments it into RTP. The offered profile is constrained baseline `42e01f`, packetization mode 1, with level asymmetry allowed. This is a synthetic negotiation choice, not an observed Allo Box profile. The caller must provide compatible parameter sets and frames; there is no transcoder or keyframe-request bridge.
- **Received device audio:** `WriteAudio` accepts mono 8 kHz PCMU or PCMA, selected when creating the peer. Its duration comes from the encoded sample count. No Opus codec is registered.
- **Browser microphone seam:** `ReadMicrophone` returns negotiated G.711 RTP packets with codec, sequence and timestamp information. A queue holds at most eight packets and drops incoming packets when full. Native PCM16/8 kHz conversion, RTP loss handling and paced talk transmission are still required. The existing native proof's `audio.go` was inspected for its A-law/mu-law boundary; none of that code or its private configuration was copied.

The [pinned Pion API](https://pkg.go.dev/github.com/pion/webrtc/v4@v4.2.26) supplies `MediaEngine.RegisterCodec`, `TrackLocalStaticSample`, `OnTrack`, `TrackRemote.ReadRTP` and `GatheringCompletePromise`. `Peer.Answer` accepts a complete offer and returns a complete gathered answer; the future Home Assistant adapter must authenticate this signaling. There is no HTTP listener or signaling service here.

## Session lifetime

The caller owns the context passed to `New`. After signaling, it must wait for `WaitConnected` before starting encoded media. Sending a single sample before both peers finish ICE/DTLS exposed intermittent sample loss during test development; waiting for both connection states removed that test failure.

Context cancellation or `Close` closes the Pion connection and waits for the RTP/RTCP readers. `Done` closes after those readers exit. Subsequent sample writes and microphone reads return cancellation. Pion's failed/closed connection states also close the peer. A transient disconnected state is not an immediate talk-stop implementation: the future session owner must cancel on lost authorization, talk release, navigation, unload or its disconnect timeout. There are no application callbacks that can outlive `Close`.

## Offline evidence

The tests constrain **both** peers to loopback ICE candidates using Pion's `SettingEngine`. They open only ephemeral loopback RTC sockets, without STUN/TURN or a publicly exposed server. The ordinary constructor configures host candidates with no external ICE servers. LAN and remote access remain unverified.

`TestSyntheticMedia` separately negotiates PCMU and PCMA and checks:

- H264 plus the selected G.711 codec arrive at the receiving peer.
- A 2,405-byte synthetic Annex B NAL survives RTP fragmentation and reassembly byte for byte. It is deliberately not a decodable video frame.
- G.711 payload bytes survive both device-audio and microphone directions.
- Parent-context cancellation completes peer teardown and rejects subsequent reads/writes.

Other tests reject unsupported Opus, empty samples, nonpositive video duration and an answer supplied as an offer. Explicit close unblocks a pending microphone read, rejects signaling/video writes and is safe to repeat.

The first test-only run failed to compile with `undefined: New` before the implementation was present. An earlier dependency setup attempt also failed because module installation was still running; that was not the behavioral red proof.

Run the focused checks from the repository root with the pinned toolchain:

```sh
mise exec go@1.26.3 -- go test -count=10 ./internal/webrtc
mise exec go@1.26.3 -- go test -race -count=10 ./internal/webrtc
mise exec go@1.26.3 -- go vet ./internal/webrtc
mise exec go@1.26.3 -- go build ./internal/webrtc
```

Equivalent checks using the installed Go 1.26.3 binary passed on Linux amd64 on 2026-11-04. Whole-module checks are the parent's integration check, since other implementation work may run concurrently.

## Evidence still required

The synthetic peers do not establish browser/iPhone decoding, actual native H264 profile compatibility, microphone codec selection on iPhone, audible live speech, full duplex, Home Assistant lifecycle/signaling, HACS packaging, a clean Container installation or remote connectivity. There is no native receive/talk bridge or microphone permission UI yet. No external STUN/TURN endpoint or additional installation prerequisite has been adopted.

Actual browser/device codec negotiation and live iPhone speech must pass before the live-intercom gate closes. Native talk pacing and cancellation must also be integrated and physically checked. LAN/remote ICE behavior must be measured through the user's actual Home Assistant access path before choosing any network configuration. Public bootstrap/trust prerequisites remain independent gates; synthetic media cannot resolve them.
