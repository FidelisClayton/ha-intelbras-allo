package media

// Talk layouts and G.711 encoding follow MIT-licensed quii-helper TCP talk/audio.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"net"
	"sync"
	"time"
)

type TalkSession struct {
	ownerMu    sync.Mutex
	closed     bool
	conn       net.Conn
	codec      *WireCodec
	ctx        context.Context
	cancel     context.CancelFunc
	stop       func() bool
	once       sync.Once
	writeMu    sync.Mutex
	channel    uint16
	audioCodec byte
	fullPacket bool
	done       chan struct{}
	pcmMu      sync.Mutex
	pending    []int16
	nextFrame  time.Time
	onClose    func()
}

func openTalk(ctx context.Context, target Target, creds Credentials, channel int) (*TalkSession, error) {
	if err := validateLANTarget(target, creds); err != nil {
		return nil, err
	}
	if channel < 0 || channel > 65535 {
		return nil, errors.New("invalid talk channel")
	}
	codec, err := NewWireCodec(creds.StreamKey())
	if err != nil {
		return nil, err
	}
	conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(target.Host, "34567"))
	if err != nil {
		return nil, errors.New("LAN talk connection failed")
	}
	return openTalkConn(ctx, conn, codec, creds, channel, 8*time.Second)
}
func talkOpen(creds Credentials, channel uint16, now time.Time) (h [32]byte, body []byte) {
	body = creds.MediaAuthBody()
	h[0] = 0x0b
	binary.LittleEndian.PutUint64(h[1:9], uint64(now.Unix()))
	binary.LittleEndian.PutUint16(h[11:13], uint16(len(body)))
	binary.LittleEndian.PutUint16(h[13:15], channel)
	return
}
func talkRequest(channel uint16, sending bool, codec byte, now time.Time) (h [32]byte) {
	h[0] = 0x0d
	if sending {
		h[0] = 0x0c
		h[14] = codec - 4
		binary.LittleEndian.PutUint16(h[15:17], 8000)
	}
	binary.LittleEndian.PutUint64(h[1:9], uint64(now.Unix()))
	binary.LittleEndian.PutUint16(h[11:13], channel)
	h[13] = 1
	return
}
func openTalkConn(ctx context.Context, conn net.Conn, codec *WireCodec, creds Credentials, channel int, keepalive time.Duration) (*TalkSession, error) {
	sessionCtx, cancel := context.WithCancel(ctx)
	s := &TalkSession{conn: conn, codec: codec, ctx: sessionCtx, cancel: cancel, channel: uint16(channel), done: make(chan struct{})}
	if s.channel == 0 {
		s.channel = 65535
	}
	s.stop = context.AfterFunc(sessionCtx, func() { _ = conn.Close() })
	fail := func(err error) (*TalkSession, error) { _ = s.Close(); return nil, err }
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := writeFull(conn, BuildSetup(2)); err != nil {
		return fail(errors.New("talk setup write failed"))
	}
	if _, err := ParseSetup(conn); err != nil {
		return fail(err)
	}
	h, body := talkOpen(creds, s.channel, time.Now())
	if err := s.command(h, body); err != nil {
		return fail(err)
	}
	pending, ready := false, false
	for count := 0; !ready; count++ {
		if count >= 16 {
			return fail(errors.New("too many talk startup messages"))
		}
		packet, err := s.readPacket()
		if err != nil {
			return fail(err)
		}
		if packet.IsMedia {
			continue
		}
		switch packet.Type {
		case 0x0b:
			if s.audioCodec != 0 {
				return fail(errors.New("unexpected talk Open reply"))
			}
			mask := binary.LittleEndian.Uint16(packet.Header[12:14])
			if mask&3 == 0 {
				return fail(errors.New("talk advertises no supported G.711 codec"))
			}
			s.audioCodec = 4
			if mask&1 == 0 {
				s.audioCodec = 5
			}
			s.fullPacket = packet.Header[14] != 0
			if err = s.command(talkRequest(s.channel, true, s.audioCodec, time.Now()), nil); err != nil {
				return fail(err)
			}
			if err = s.command(talkRequest(s.channel, false, s.audioCodec, time.Now()), nil); err != nil {
				return fail(err)
			}
			pending = true
		case 0x0d:
			if !pending {
				return fail(errors.New("unexpected talk ready reply"))
			}
			ready = true
		}
	}
	_ = conn.SetDeadline(time.Time{})
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(keepalive)
		defer ticker.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-ticker.C:
				if s.command(liveHeader(0), nil) != nil {
					_ = s.Close()
					return
				}
			}
		}
	}()
	return s, nil
}
func (s *TalkSession) write(packet []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.ctx.Err(); err != nil {
		return err
	}
	_ = s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := writeFull(s.conn, packet); err != nil {
		s.cancel()
		return errors.New("talk write failed")
	}
	return nil
}
func (s *TalkSession) command(h [32]byte, body []byte) error {
	packet, err := s.codec.EncodeCommand(h, body)
	if err != nil {
		return err
	}
	return s.write(packet)
}

// Validate the tighter talk bounds before WireCodec allocates the body.
func (s *TalkSession) readPacket() (WirePacket, error) {
	var encrypted [32]byte
	if _, err := io.ReadFull(s.conn, encrypted[:]); err != nil {
		return WirePacket{}, errors.New("talk read failed")
	}
	h := s.codec.crypt(encrypted[:], true)
	size := int(binary.LittleEndian.Uint16(h[9:11]))
	media := wireMediaType(h[0])
	if media {
		size = int(binary.LittleEndian.Uint32(h[11:15]))
		if size < 1 || size > 65555 {
			return WirePacket{}, errors.New("invalid talk media length")
		}
	} else if (h[0] != 0 && h[0] != 0x0b && h[0] != 0x0c && h[0] != 0x0d) || size < 32 || size > 4096 || size%16 != 0 {
		return WirePacket{}, errors.New("invalid talk command header")
	}
	p, err := s.codec.ReadPacket(io.MultiReader(bytes.NewReader(encrypted[:]), s.conn))
	if err != nil {
		return p, err
	}
	if !p.IsMedia {
		sum := sha256.Sum256(p.Header[:])
		if subtle.ConstantTimeCompare(sum[:], p.Payload[:32]) != 1 {
			return p, errors.New("talk command SHA256 check failed")
		}
		if p.Type != 0 && p.Header[11] != 0 {
			return p, fmt.Errorf("talk command 0x%02x rejected with status %d", p.Type, p.Header[11])
		}
	}
	return p, nil
}
func (s *TalkSession) Close() error {
	var err error
	s.once.Do(func() {
		s.cancel()
		if s.stop != nil {
			s.stop()
		}
		err = s.conn.Close()
		s.writeMu.Lock()
		s.writeMu.Unlock()
		s.ownerMu.Lock()
		s.closed = true
		release := s.onClose
		s.ownerMu.Unlock()
		if release != nil {
			release()
		}
	})
	return err
}

func encodeG711(pcm []int16, codec byte) []byte {
	result := make([]byte, len(pcm))
	for i, v := range pcm {
		sample := int(v)
		mask := 0xd5
		value := sample
		segment := 0
		encoded := 0
		if codec == 4 {
			if sample < 0 {
				mask = 0x55
				value = -sample - 1
			}
			segment = max(0, bits.Len(uint(value))-8)
			shift := 4
			if segment > 0 {
				shift = segment + 3
			}
			encoded = segment<<4 | (value>>shift)&15
		} else {
			sample >>= 2
			mask = 0xff
			if sample < 0 {
				mask = 0x7f
				sample = -sample
			}
			value = sample + 33
			segment = max(0, bits.Len(uint(value))-6)
			encoded = 0x7f
			if segment < 8 {
				encoded = segment<<4 | (value>>(segment+1))&15
			}
		}
		result[i] = byte(encoded ^ mask)
	}
	return result
}
func (s *TalkSession) audioPacket(encoded []byte, now time.Time) ([]byte, error) {
	if len(encoded) != 320 || (s.audioCodec != 4 && s.audioCodec != 5) {
		return nil, errors.New("talk requires 320 G.711 samples")
	}
	prefix := 8
	if s.fullPacket {
		prefix = 20
	}
	body := make([]byte, prefix+320)
	copy(body, []byte{0, 0, 1, 0xf0})
	if s.fullPacket {
		if now.Year() < 2000 || now.Year() > 2063 {
			return nil, errors.New("talk timestamp outside native range")
		}
		body[3] = 0xe3
		binary.LittleEndian.PutUint32(body[4:8], 320)
		stamp := uint32(now.Year()-2000)<<26 | uint32(now.Month())<<22 | uint32(now.Day())<<17 | uint32(now.Hour())<<12 | uint32(now.Minute())<<6 | uint32(now.Second())
		binary.LittleEndian.PutUint32(body[8:12], stamp)
		binary.LittleEndian.PutUint16(body[12:14], uint16(now.Nanosecond()/1e6))
		body[14] = s.audioCodec
		body[15] = 1
		binary.LittleEndian.PutUint16(body[16:18], 8000)
	} else {
		body[4] = 0x0e
		if s.audioCodec == 5 {
			body[4] = 0x0a
		}
		body[5] = 2
		binary.LittleEndian.PutUint16(body[6:8], 320)
	}
	copy(body[prefix:], encoded)
	h := liveHeader(0xa2)
	binary.LittleEndian.PutUint64(h[1:9], uint64(now.Unix()))
	return s.codec.BuildMedia(h, body, 32)
}

// WritePCM accepts live PCM16 mono 8000Hz samples. Complete 320-sample native
// packets are paced at 40ms across calls; Close discards a remaining partial packet.
func (s *TalkSession) WritePCM(pcm []int16) error {
	s.pcmMu.Lock()
	defer s.pcmMu.Unlock()
	if err := s.ctx.Err(); err != nil {
		return err
	}
	for len(pcm) > 0 {
		n := min(320-len(s.pending), len(pcm))
		s.pending = append(s.pending, pcm[:n]...)
		pcm = pcm[n:]
		if len(s.pending) < 320 {
			continue
		}
		timer := time.NewTimer(time.Until(s.nextFrame))
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return s.ctx.Err()
		case <-timer.C:
		}
		packet, err := s.audioPacket(encodeG711(s.pending, s.audioCodec), time.Now())
		if err != nil {
			return err
		}
		if err = s.write(packet); err != nil {
			return err
		}
		s.pending = s.pending[:0]
		// A slow write moves the schedule forward instead of bursting queued frames.
		s.nextFrame = time.Now().Add(40 * time.Millisecond)
	}
	return nil
}

func (s *TalkSession) setOwner(release func()) {
	s.ownerMu.Lock()
	s.onClose = release
	closed := s.closed
	s.ownerMu.Unlock()
	if closed {
		release()
	}
}
