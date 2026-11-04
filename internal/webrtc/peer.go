// Package webrtc bridges encoded intercom samples and a negotiated G.711 microphone.
// Signaling authentication, native framing and talk pacing belong to the caller.
package webrtc

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/pion/rtp"
	pion "github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// MicrophonePacket retains the negotiated codec and RTP timing for the talk bridge.
type MicrophonePacket struct {
	Codec  string
	Packet *rtp.Packet
}

// Peer owns one WebRTC session. Canceling its context or calling Close stops it.
// Samples are already encoded; this package neither transcodes nor paces native talk.
type Peer struct {
	pc           *pion.PeerConnection
	video, audio *pion.TrackLocalStaticSample
	ctx          context.Context
	cancel       context.CancelFunc
	mic          chan MicrophonePacket
	done         chan struct{}
	connected    chan struct{}
	once         sync.Once
	mu           sync.Mutex
	closing      bool
	readers      sync.WaitGroup
	closeErr     error
}

// New creates a host-candidate-only peer with H264 and one G.711 audio codec.
// No external STUN/TURN service is configured. Remote connectivity is unverified.
func New(parent context.Context, audioCodec string) (*Peer, error) {
	if parent == nil {
		return nil, errors.New("context is required")
	}
	if audioCodec != pion.MimeTypePCMU && audioCodec != pion.MimeTypePCMA {
		return nil, errors.New("audio must be audio/PCMU or audio/PCMA")
	}
	return newPeer(parent, audioCodec, pion.SettingEngine{})
}

func newPeer(parent context.Context, audioCodec string, settings pion.SettingEngine) (*Peer, error) {
	engine := &pion.MediaEngine{}
	audio := pion.RTPCodecCapability{MimeType: audioCodec, ClockRate: 8000, Channels: 1}
	payload := pion.PayloadType(0)
	if audioCodec == pion.MimeTypePCMA {
		payload = 8
	}
	if err := engine.RegisterCodec(pion.RTPCodecParameters{RTPCodecCapability: audio, PayloadType: payload}, pion.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	video := pion.RTPCodecCapability{MimeType: pion.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"}
	if err := engine.RegisterCodec(pion.RTPCodecParameters{RTPCodecCapability: video, PayloadType: 102}, pion.RTPCodecTypeVideo); err != nil {
		return nil, err
	}
	pc, err := pion.NewAPI(pion.WithMediaEngine(engine), pion.WithSettingEngine(settings)).NewPeerConnection(pion.Configuration{})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	p := &Peer{pc: pc, ctx: ctx, cancel: cancel, mic: make(chan MicrophonePacket, 8), done: make(chan struct{}), connected: make(chan struct{})}
	p.video, err = pion.NewTrackLocalStaticSample(video, "video", "intercom")
	if err != nil {
		p.Close()
		return nil, err
	}
	p.audio, err = pion.NewTrackLocalStaticSample(audio, "audio", "intercom")
	if err != nil {
		p.Close()
		return nil, err
	}
	for _, track := range []*pion.TrackLocalStaticSample{p.video, p.audio} {
		sender, addErr := pc.AddTrack(track)
		if addErr != nil {
			p.Close()
			return nil, addErr
		}
		p.readers.Add(1)
		go func() {
			defer p.readers.Done()
			buffer := make([]byte, 1500)
			for {
				if _, _, err := sender.Read(buffer); err != nil {
					return
				}
			}
		}()
	}
	pc.OnTrack(func(track *pion.TrackRemote, _ *pion.RTPReceiver) {
		if track.Kind() != pion.RTPCodecTypeAudio || track.Codec().MimeType != audioCodec {
			return
		}
		p.mu.Lock()
		if p.closing {
			p.mu.Unlock()
			return
		}
		p.readers.Add(1)
		p.mu.Unlock()
		go func() {
			defer p.readers.Done()
			for {
				packet, _, err := track.ReadRTP()
				if err != nil {
					return
				}
				if ctx.Err() != nil {
					return
				}
				// A slow native talk consumer must not accumulate unbounded microphone audio.
				select {
				case p.mic <- MicrophonePacket{Codec: track.Codec().MimeType, Packet: packet}:
				default:
				}
			}
		}()
	})
	var connectedOnce sync.Once
	pc.OnConnectionStateChange(func(state pion.PeerConnectionState) {
		if state == pion.PeerConnectionStateConnected {
			connectedOnce.Do(func() { close(p.connected) })
		}
		if state == pion.PeerConnectionStateFailed || state == pion.PeerConnectionStateClosed {
			go p.Close()
		}
	})
	context.AfterFunc(ctx, func() { p.Close() })
	return p, nil
}

// Answer accepts a complete offer and returns an answer with gathered candidates.
// The caller must authenticate signaling before calling this method.
func (p *Peer) Answer(ctx context.Context, offer pion.SessionDescription) (pion.SessionDescription, error) {
	if err := p.ctx.Err(); err != nil {
		return pion.SessionDescription{}, err
	}
	if offer.Type != pion.SDPTypeOffer {
		return pion.SessionDescription{}, errors.New("expected SDP offer")
	}
	if err := p.pc.SetRemoteDescription(offer); err != nil {
		return pion.SessionDescription{}, err
	}
	answer, err := p.pc.CreateAnswer(nil)
	if err != nil {
		return pion.SessionDescription{}, err
	}
	gathered := pion.GatheringCompletePromise(p.pc)
	if err = p.pc.SetLocalDescription(answer); err != nil {
		return pion.SessionDescription{}, err
	}
	select {
	case <-ctx.Done():
		return pion.SessionDescription{}, ctx.Err()
	case <-p.ctx.Done():
		return pion.SessionDescription{}, p.ctx.Err()
	case <-gathered:
		return *p.pc.LocalDescription(), nil
	}
}

// WaitConnected waits until ICE/DTLS is ready before the caller starts media.
func (p *Peer) WaitConnected(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.ctx.Done():
		return p.ctx.Err()
	case <-p.connected:
		return p.ctx.Err()
	}
}

// WriteVideo packetizes one complete Annex B H264 access unit. The caller supplies
// its duration and compatible SPS/PPS; no decoder/profile conversion is performed.
func (p *Peer) WriteVideo(annexB []byte, duration time.Duration) error {
	if err := p.ctx.Err(); err != nil {
		return err
	}
	if len(annexB) == 0 || duration <= 0 {
		return errors.New("video sample and positive duration required")
	}
	return p.video.WriteSample(media.Sample{Data: annexB, Duration: duration})
}

// WriteAudio packetizes mono 8 kHz G.711 bytes in the codec selected by New.
func (p *Peer) WriteAudio(encoded []byte) error {
	if err := p.ctx.Err(); err != nil {
		return err
	}
	if len(encoded) == 0 {
		return errors.New("audio sample required")
	}
	return p.audio.WriteSample(media.Sample{Data: encoded, Duration: time.Duration(len(encoded)) * time.Second / 8000})
}

// ReadMicrophone returns encoded G.711 RTP. Its bounded queue drops new packets
// when full. The native talk bridge must handle RTP loss and pace decoded PCM.
func (p *Peer) ReadMicrophone(ctx context.Context) (MicrophonePacket, error) {
	select {
	case <-ctx.Done():
		return MicrophonePacket{}, ctx.Err()
	case <-p.ctx.Done():
		return MicrophonePacket{}, p.ctx.Err()
	case packet := <-p.mic:
		if err := p.ctx.Err(); err != nil {
			return MicrophonePacket{}, err
		}
		return packet, nil
	}
}

// Done closes after the connection and all media readers have stopped.
func (p *Peer) Done() <-chan struct{} { return p.done }

// Close is idempotent and waits for all media readers to exit.
func (p *Peer) Close() error {
	p.once.Do(func() {
		p.mu.Lock()
		p.closing = true
		p.mu.Unlock()
		p.cancel()
		p.closeErr = p.pc.Close()
		p.readers.Wait()
		close(p.done)
	})
	return p.closeErr
}
