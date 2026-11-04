package media

// Protocol layouts adapted from the MIT-licensed quii-helper TCP live session.
import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

type liveSession struct {
	conn     net.Conn
	codec    *WireCodec
	ctx      context.Context
	cancel   context.CancelFunc
	stop     func() bool
	writeMu  sync.Mutex
	once     sync.Once
	accepted atomic.Bool
	done     chan struct{}
	ready    chan struct{}
}

func validateLANTarget(target Target, creds Credentials) error {
	ip := net.ParseIP(target.Host)
	if ip == nil || !ip.IsPrivate() || ip.IsMulticast() {
		return errors.New("LAN target must be an explicit private IP")
	}
	if target.DeviceID == "" || target.DeviceID != creds.DeviceID() {
		return errors.New("LAN target identity does not match selected media credentials")
	}
	body := creds.MediaAuthBody()
	if len(body) == 0 || len(body) > 65520-32 {
		return errors.New("invalid media authentication body")
	}
	return nil
}

func openLive(ctx context.Context, target Target, creds Credentials, channel, stream int) (*liveSession, error) {
	if err := validateLANTarget(target, creds); err != nil {
		return nil, err
	}
	if channel < 1 || channel > 65535 || stream < 1 || stream > 256 {
		return nil, errors.New("invalid channel or stream")
	}
	codec, err := NewWireCodec(creds.StreamKey())
	if err != nil {
		return nil, err
	}
	conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(target.Host, "34567"))
	if err != nil {
		return nil, errors.New("LAN TCP connection failed")
	}
	return openLiveConn(ctx, conn, codec, creds, channel, stream, 8*time.Second)
}

func openLiveConn(ctx context.Context, conn net.Conn, codec *WireCodec, creds Credentials, channel, stream int, keepalive time.Duration) (*liveSession, error) {
	sessionCtx, cancel := context.WithCancel(ctx)
	s := &liveSession{conn: conn, codec: codec, ctx: sessionCtx, cancel: cancel, done: make(chan struct{}), ready: make(chan struct{})}
	s.stop = context.AfterFunc(sessionCtx, func() { _ = conn.Close() })
	fail := func(err error) (*liveSession, error) { _ = s.Close(); return nil, err }
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := writeFull(conn, BuildSetup(0)); err != nil {
		return fail(errors.New("LAN setup write failed"))
	}
	if _, err := ParseSetup(conn); err != nil {
		return fail(err)
	}
	h, body := livePlayCommand(creds, channel, stream, uint64(time.Now().Unix()))
	if err := s.SendCommand(h, body); err != nil {
		return fail(err)
	}
	_ = conn.SetDeadline(time.Time{})
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(keepalive)
		defer ticker.Stop()
		for {
			select {
			case <-sessionCtx.Done():
				return
			case <-ticker.C:
				if s.accepted.Load() {
					if err := s.SendCommand(liveHeader(0), nil); err != nil {
						_ = s.Close()
						return
					}
				}
			}
		}
	}()
	return s, nil
}

func livePlayCommand(creds Credentials, channel, stream int, stamp uint64) (h [32]byte, body []byte) {
	body = creds.MediaAuthBody()
	h[0] = 1
	binary.LittleEndian.PutUint64(h[1:9], stamp)
	binary.LittleEndian.PutUint16(h[11:13], uint16(len(body)))
	binary.LittleEndian.PutUint16(h[13:15], uint16(channel))
	h[15] = 1
	h[16] = byte(stream - 1)
	return
}

func liveHeader(kind byte) (h [32]byte) {
	h[0] = kind
	binary.LittleEndian.PutUint64(h[1:9], uint64(time.Now().Unix()))
	return
}
func writeFull(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
func (s *liveSession) SendCommand(h [32]byte, body []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.ctx.Err() != nil {
		return s.ctx.Err()
	}
	b, err := s.codec.EncodeCommand(h, body)
	if err != nil {
		return err
	}
	_ = s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err = writeFull(s.conn, b); err != nil {
		_ = s.Close()
		return errors.New("LAN command write failed")
	}
	return nil
}
func (s *liveSession) NextPacket() (WirePacket, error) {
	_ = s.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	packet, err := s.codec.ReadPacket(s.conn)
	if err == nil && !packet.IsMedia {
		err = s.checkCommand(packet)
	}
	if err != nil {
		contextErr := s.ctx.Err()
		_ = s.Close()
		if contextErr != nil {
			return WirePacket{}, contextErr
		}
		return WirePacket{}, err
	}
	return packet, nil
}
func (s *liveSession) checkCommand(p WirePacket) error {
	switch p.Type {
	case 0, 1, 5, 6, 7, 8, 9, 10, 12, 13, 14, 15, 17, 0xaa, 0xfe:
	default:
		return errors.New("unsupported LAN command type")
	}
	size := 0
	if p.Type == 0xfe {
		size = int(binary.LittleEndian.Uint16(p.Header[11:13]))
	}
	if size+32 > len(p.Payload) {
		return errors.New("invalid LAN command digest size")
	}
	hash := sha256.New()
	hash.Write(p.Header[:])
	hash.Write(p.Payload[:size])
	if subtle.ConstantTimeCompare(hash.Sum(nil), p.Payload[size:size+32]) != 1 {
		return errors.New("LAN command SHA256 check failed")
	}
	switch p.Type {
	case 1, 12, 13, 17, 0xaa:
		if p.Header[11] != 0 {
			return fmt.Errorf("LAN play rejected with status %d", p.Header[11])
		}
		if p.Header[12] != 1 {
			return errors.New("LAN play was not accepted")
		}
		if !s.accepted.Swap(true) {
			close(s.ready)
		}
	}
	return nil
}
func (s *liveSession) Close() error {
	var err error
	s.once.Do(func() {
		s.cancel()
		if s.stop != nil {
			s.stop()
		}
		err = s.conn.Close()
	})
	return err
}
