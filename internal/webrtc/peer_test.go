package webrtc

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/pion/rtp/codecs"
	pion "github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

func TestSyntheticMedia(t *testing.T) {
	for _, codec := range []string{pion.MimeTypePCMU, pion.MimeTypePCMA} {
		t.Run(codec, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			settings := pion.SettingEngine{}
			settings.SetIncludeLoopbackCandidate(true)
			settings.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
			p, err := newPeer(ctx, codec, settings)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			remote, err := pion.NewAPI(pion.WithSettingEngine(settings)).NewPeerConnection(pion.Configuration{})
			if err != nil {
				t.Fatal(err)
			}
			defer remote.Close()
			mic, err := pion.NewTrackLocalStaticSample(pion.RTPCodecCapability{MimeType: codec, ClockRate: 8000, Channels: 1}, "mic", "synthetic")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = remote.AddTrack(mic); err != nil {
				t.Fatal(err)
			}
			if _, err = remote.AddTransceiverFromKind(pion.RTPCodecTypeVideo, pion.RTPTransceiverInit{Direction: pion.RTPTransceiverDirectionRecvonly}); err != nil {
				t.Fatal(err)
			}
			type receivedSample struct {
				codec string
				data  []byte
			}
			received := make(chan receivedSample, 2)
			remote.OnTrack(func(track *pion.TrackRemote, _ *pion.RTPReceiver) {
				var data []byte
				h264 := &codecs.H264Packet{}
				for {
					packet, _, err := track.ReadRTP()
					if err != nil {
						return
					}
					if track.Kind() == pion.RTPCodecTypeVideo {
						fragment, err := h264.Unmarshal(packet.Payload)
						if err != nil {
							return
						}
						data = append(data, fragment...)
					} else {
						data = append(data, packet.Payload...)
					}
					if packet.Marker || track.Kind() == pion.RTPCodecTypeAudio {
						break
					}
				}
				received <- receivedSample{track.Codec().MimeType, data}
			})
			offer, err := remote.CreateOffer(nil)
			if err != nil {
				t.Fatal(err)
			}
			gathered := pion.GatheringCompletePromise(remote)
			if err = remote.SetLocalDescription(offer); err != nil {
				t.Fatal(err)
			}
			select {
			case <-gathered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			answer, err := p.Answer(ctx, *remote.LocalDescription())
			if err != nil {
				t.Fatal(err)
			}
			if err = remote.SetRemoteDescription(answer); err != nil {
				t.Fatal(err)
			}
			connected := make(chan struct{}, 1)
			remote.OnConnectionStateChange(func(state pion.PeerConnectionState) {
				if state == pion.PeerConnectionStateConnected {
					select {
					case connected <- struct{}{}:
					default:
					}
				}
			})
			if remote.ConnectionState() != pion.PeerConnectionStateConnected {
				select {
				case <-connected:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			if err = p.WaitConnected(ctx); err != nil {
				t.Fatal(err)
			}
			// Large synthetic IDR NAL exercises Pion's H264 RTP fragmentation; not a decodable frame.
			frame := append([]byte{0, 0, 0, 1, 0x65}, bytes.Repeat([]byte{0x42}, 2400)...)
			audio := bytes.Repeat([]byte{0x7f}, 160)
			if err = p.WriteVideo(frame, time.Second/25); err != nil {
				t.Fatal(err)
			}
			if err = p.WriteAudio(audio); err != nil {
				t.Fatal(err)
			}
			if err = mic.WriteSample(media.Sample{Data: audio, Duration: 20 * time.Millisecond}); err != nil {
				t.Fatal(err)
			}
			packet, err := p.ReadMicrophone(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if packet.Codec != codec || !bytes.Equal(packet.Packet.Payload, audio) {
				t.Fatalf("microphone codec/payload mismatch: %s", packet.Codec)
			}
			seen := map[string]bool{}
			for len(seen) < 2 {
				select {
				case sample := <-received:
					seen[sample.codec] = true
					expected := audio
					if sample.codec == pion.MimeTypeH264 {
						expected = frame
					}
					if !bytes.Equal(sample.data, expected) {
						t.Fatalf("received %s sample differs: got %d bytes want %d", sample.codec, len(sample.data), len(expected))
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			if !seen[pion.MimeTypeH264] || !seen[codec] {
				t.Fatalf("unexpected negotiated codecs %v", seen)
			}
			cancel()
			select {
			case <-p.Done():
			case <-time.After(3 * time.Second):
				t.Fatal("cancel did not close peer")
			}
			if _, err = p.ReadMicrophone(context.Background()); !errors.Is(err, context.Canceled) {
				t.Fatalf("read after close: %v", err)
			}
			if err = p.WriteAudio(audio); !errors.Is(err, context.Canceled) {
				t.Fatalf("write after close: %v", err)
			}
			if err = p.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRejectUnsupportedAudio(t *testing.T) {
	if p, err := New(context.Background(), pion.MimeTypeOpus); err == nil {
		p.Close()
		t.Fatal("accepted Opus without transcoder")
	}
}

func TestCloseStopsReadAndRejectsWrites(t *testing.T) {
	p, err := New(context.Background(), pion.MimeTypePCMU)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	read := make(chan error, 1)
	go func() { _, err := p.ReadMicrophone(context.Background()); read <- err }()
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-read:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("blocked read: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("close left read blocked")
	}
	if err = p.WriteVideo([]byte{0, 0, 1, 0x65}, time.Second/25); !errors.Is(err, context.Canceled) {
		t.Fatalf("video after close: %v", err)
	}
	if _, err = p.Answer(context.Background(), pion.SessionDescription{Type: pion.SDPTypeOffer}); !errors.Is(err, context.Canceled) {
		t.Fatalf("answer after close: %v", err)
	}
}

func TestRejectInvalidSamplesAndSignaling(t *testing.T) {
	p, err := New(context.Background(), pion.MimeTypePCMA)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err = p.WriteVideo(nil, time.Second); err == nil {
		t.Fatal("accepted empty video")
	}
	if err = p.WriteVideo([]byte{0, 0, 1, 0x65}, 0); err == nil {
		t.Fatal("accepted nonpositive duration")
	}
	if err = p.WriteAudio(nil); err == nil {
		t.Fatal("accepted empty audio")
	}
	if _, err = p.Answer(context.Background(), pion.SessionDescription{Type: pion.SDPTypeAnswer}); err == nil {
		t.Fatal("accepted answer as offer")
	}
}
