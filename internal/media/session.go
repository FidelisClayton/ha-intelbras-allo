// Package media streams the native Allo LAN protocol. It performs no discovery,
// cloud authentication, recording or device configuration.
package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"time"
)

// Credentials preserves the bootstrap authentication body as opaque bytes.
type Credentials interface {
	DeviceID() string
	StreamKey() string
	MediaAuthBody() []byte
}

type Target struct{ DeviceID, Host string }

const (
	CodecH264  = "H264"
	CodecPCM16 = "PCM16"
)

// Frame contains an H264 Annex B access unit or little-endian PCM16 mono 8 kHz.
// Duration comes from native FPS for video and decoded sample count for audio.
type Frame struct {
	Codec    string
	Data     []byte
	Duration time.Duration
}

// Session owns the sole receive reader and at most one microphone transmitter.
// ReadFrame may be called by one consumer. Slow consumers lose frames; reception
// resumes video at an IDR so a queued interframe cannot reference dropped data.
type Session struct {
	live    *liveSession
	target  Target
	creds   Credentials
	frames  chan Frame
	done    chan struct{}
	err     error
	talkMu  sync.Mutex
	talking bool
	talk    *TalkSession
}

func Open(ctx context.Context, target Target, creds Credentials, channel, stream int) (*Session, error) {
	if creds == nil {
		return nil, errors.New("media credentials required")
	}
	live, err := openLive(ctx, target, creds, channel, stream)
	if err != nil {
		return nil, err
	}
	return startSession(live, target, creds), nil
}

func startSession(live *liveSession, target Target, creds Credentials) *Session {
	s := &Session{live: live, target: target, creds: creds, frames: make(chan Frame, 32), done: make(chan struct{})}
	go s.receive()
	return s
}

func (s *Session) receive() {
	defer close(s.done)
	defer s.Close()
	defer close(s.frames)
	var assembler CPacketAssembler
	var video h264CaptureState
	for {
		p, err := s.live.NextPacket()
		if err != nil {
			s.err = err
			return
		}
		if !p.IsMedia {
			continue
		}
		packets, err := assembler.Feed(p.Payload)
		if err != nil {
			s.err = err
			return
		}
		for _, packet := range packets {
			frame, err := decodeFrame(packet, &video)
			if err != nil {
				s.err = err
				return
			}
			if len(frame.Data) == 0 {
				continue
			}
			select {
			case s.frames <- frame:
			default:
				video.started = false
			}
		}
	}
}

func decodeFrame(packet MediaFrame, video *h264CaptureState) (Frame, error) {
	h := packet.Header
	if h[3] == 0xe3 {
		channels := h[15]
		if (channels != 0 && channels != 1) || binary.LittleEndian.Uint16(h[16:18]) != 8000 {
			return Frame{}, errors.New("audio requires mono 8000Hz")
		}
		pcm, err := decodeAudio(h[14], packet.Data)
		if err != nil {
			return Frame{}, err
		}
		return Frame{CodecPCM16, pcm, time.Duration(len(pcm)/2) * time.Second / 8000}, nil
	}
	kind := (h[3] + 0x20) & 0xff
	if kind != 0 && kind != 1 && kind != 9 && kind != 10 {
		return Frame{}, nil
	}
	if h[14] != 1 && (h[14] < 0x10 || h[14] > 0x1f) {
		return Frame{}, errors.New("unsupported video codec")
	}
	offset := bytes.Index(packet.Data, []byte{0, 0, 0, 1})
	if offset < 0 {
		offset = bytes.Index(packet.Data, []byte{0, 0, 1})
	}
	if offset < 0 {
		return Frame{}, errors.New("H264 frame has no Annex B start code")
	}
	if h[15] != 0 {
		video.fps = h[15]
	}
	data, picture := video.accept(packet.Data[offset:])
	if !picture {
		return Frame{}, nil
	}
	fps := int(video.fps)
	if fps == 0 {
		return Frame{}, errors.New("unsupported video timing: no native FPS advertised")
	}
	return Frame{CodecH264, data, 4 * time.Second / time.Duration(fps)}, nil
}

func (s *Session) ReadFrame(ctx context.Context) (Frame, error) {
	select {
	case <-ctx.Done():
		return Frame{}, ctx.Err()
	case frame, ok := <-s.frames:
		if !ok {
			<-s.done
			return Frame{}, s.err
		}
		return frame, nil
	}
}

func (s *Session) OpenTalk(ctx context.Context) (*TalkSession, error) {
	return s.openTalk(ctx, func(ctx context.Context) (*TalkSession, error) { return openTalk(ctx, s.target, s.creds, 0) })
}

func (s *Session) openTalk(ctx context.Context, open func(context.Context) (*TalkSession, error)) (*TalkSession, error) {
	s.talkMu.Lock()
	if s.talking {
		s.talkMu.Unlock()
		return nil, errors.New("talk already owned")
	}
	s.talking = true
	s.talkMu.Unlock()
	release := func() { s.talkMu.Lock(); s.talking = false; s.talk = nil; s.talkMu.Unlock() }
	talkCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.live.ctx, cancel)
	fail := func(err error) (*TalkSession, error) { stop(); cancel(); release(); return nil, err }
	select {
	case <-talkCtx.Done():
		return fail(talkCtx.Err())
	case <-s.live.ready:
	}
	talk, err := open(talkCtx)
	if err != nil {
		return fail(err)
	}
	// Install ownership before starting the cancellation observer.
	s.talkMu.Lock()
	s.talk = talk
	s.talkMu.Unlock()
	talk.setOwner(func() { stop(); cancel(); release() })
	go func() {
		defer talk.Close()
		for {
			p, err := talk.readPacket()
			if err != nil || (!p.IsMedia && p.Type == 0x0b) {
				return
			}
		}
	}()
	return talk, nil
}

func (s *Session) Close() error {
	err := s.live.Close()
	s.talkMu.Lock()
	talk := s.talk
	s.talkMu.Unlock()
	if talk != nil {
		_ = talk.Close()
	}
	return err
}
