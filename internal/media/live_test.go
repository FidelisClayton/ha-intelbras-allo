package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func testCodec(t *testing.T) *WireCodec {
	t.Helper()
	c, err := NewWireCodec(strings.Repeat("k", 32))
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func setupFake(t *testing.T, c *WireCodec, server net.Conn, replyStatus byte, keepalive bool) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		defer server.Close()
		h := make([]byte, 32)
		_, err := io.ReadFull(server, h)
		if err != nil {
			done <- err
			return
		}
		if h[0] != 0xa9 || h[9] != 0 {
			done <- io.ErrUnexpectedEOF
			return
		}
		h[10] = 2
		h[11] = 1
		if err = writeFull(server, h); err != nil {
			done <- err
			return
		}
		play, err := c.ReadPacket(server)
		if err != nil {
			done <- err
			return
		}
		size := int(binary.LittleEndian.Uint16(play.Header[11:13]))
		sum := sha256.Sum256(append(play.Header[:], play.Payload[:size]...))
		if play.Type != 1 || play.Header[15] != 1 || binary.LittleEndian.Uint16(play.Header[13:15]) != 1 || play.Header[16] != 1 || string(play.Payload[:size]) != "viewer&&opaque&password/+==\x00OEM123\x000123456789abcdef\x00" || string(sum[:]) != string(play.Payload[size:size+32]) {
			done <- io.ErrUnexpectedEOF
			return
		}
		var reply [32]byte
		reply[0] = 1
		reply[11] = replyStatus
		reply[12] = 1
		packet, err := c.EncodeCommand(reply, nil)
		if err == nil {
			err = writeFull(server, packet)
		}
		if err != nil {
			done <- err
			return
		}
		if keepalive {
			p, err := c.ReadPacket(server)
			if err != nil {
				done <- err
				return
			}
			if p.Type != 0 || p.Header[15] != 0 {
				done <- io.ErrUnexpectedEOF
				return
			}
		}
		done <- nil
	}()
	return done
}
func TestLivePlayKeepaliveAndBusy(t *testing.T) {
	for _, status := range []byte{0, 5} {
		t.Run(string(rune('a'+status)), func(t *testing.T) {
			client, server := net.Pipe()
			codec := testCodec(t)
			done := setupFake(t, codec, server, status, status == 0)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			s, err := openLiveConn(ctx, client, codec, syntheticMediaCredentials(), 1, 2, time.Millisecond*5)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			_, err = s.NextPacket()
			if status == 0 && err != nil {
				t.Fatal(err)
			}
			if status != 0 && (err == nil || !strings.Contains(err.Error(), "status 5")) {
				t.Fatalf("busy status lost: %v", err)
			}
			if err = <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestLiveCancellationPartialPacket(t *testing.T) {
	client, server := net.Pipe()
	codec := testCodec(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := setupFake(t, codec, server, 0, false)
	s, err := openLiveConn(ctx, client, codec, syntheticMediaCredentials(), 1, 2, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.NextPacket(); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = s.Close()
	select {
	case <-s.done:
	case <-time.After(time.Second):
		t.Fatal("keepalive did not stop")
	}
}
func TestLiveCancelWhileHeaderPartial(t *testing.T) {
	client, server := net.Pipe()
	codec := testCodec(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		defer server.Close()
		h := make([]byte, 32)
		_, _ = io.ReadFull(server, h)
		h[10] = 2
		h[11] = 1
		_ = writeFull(server, h)
		_, _ = codec.ReadPacket(server)
		_, _ = server.Write([]byte{1, 2, 3})
		<-ctx.Done()
	}()
	s, err := openLiveConn(ctx, client, codec, syntheticMediaCredentials(), 1, 2, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	read := make(chan error, 1)
	go func() { _, e := s.NextPacket(); read <- e }()
	cancel()
	select {
	case err = <-read:
		if err == nil {
			t.Fatal("canceled read succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled read blocked")
	}
	select {
	case <-s.done:
	case <-time.After(time.Second):
		t.Fatal("keepalive goroutine leaked")
	}
}
func TestLiveRejectsBadDigest(t *testing.T) {
	c := testCodec(t)
	s := &liveSession{}
	p := WirePacket{Type: 1, Payload: make([]byte, 32)}
	p.Header[0] = 1
	p.Header[12] = 1
	if err := s.checkCommand(p); err == nil {
		t.Fatal("bad digest accepted")
	}
	_ = c
}

// Legacy quii-helper PLAY fixture tests general wire encoding, not native authentication.
func TestWireCodecLegacyUpstreamPacketFixture(t *testing.T) {
	expected, err := hex.DecodeString("194d4809a2bed155e8758b9c1e8a5c25e070848982ec42ae6e907fc736cbfce37389f689637e58ffc5ff6c9bcb0f837e46fa5c0949f40a2342ca1c47c4a60ab024171d94de7201b7ac13c301b7fe8b246f890c8ab427e9c85084ccb4b79124258cca1a0ee0110c23cb57f2323b18d3eaaf011791fa407f11219849f0e7c02d6ca18d4ab64bf2286ce3d129e95de6b6854d6f315246bb9866025c0d948d821ba23ab10ddf243bd0569f792b5d1402b222")
	if err != nil {
		t.Fatal(err)
	}
	var header [32]byte
	body := []byte("adminapp2&&" + strings.Repeat("a", 64) + "\x00/mode=real&idc=1&ids=2&ap=2\x00")
	header[0], header[15], header[16] = 1, 1, 1
	binary.LittleEndian.PutUint64(header[1:9], 1700000000)
	binary.LittleEndian.PutUint16(header[11:13], uint16(len(body)))
	binary.LittleEndian.PutUint16(header[13:15], 1)
	actual, err := testCodec(t).EncodeCommand(header, body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatal("live open differs from independent upstream fixture")
	}
}

func TestNativeMediaCredentialBody(t *testing.T) {
	lh, live := livePlayCommand(syntheticMediaCredentials(), 1, 2, 1700000000)
	th, talk := talkOpen(syntheticMediaCredentials(), 65535, time.Unix(1700000000, 0))
	want := []byte("viewer&&opaque&password/+==\x00OEM123\x000123456789abcdef\x00")
	if binary.LittleEndian.Uint16(lh[11:13]) != uint16(len(want)) || binary.LittleEndian.Uint16(th[11:13]) != uint16(len(want)) || lh[0] != 1 || th[0] != 0x0b || lh[15] != 1 || lh[16] != 1 || lh[17] != 0 || binary.LittleEndian.Uint16(lh[13:15]) != 1 || binary.LittleEndian.Uint16(th[13:15]) != 65535 || bytes.Count(live, []byte{0}) != 3 {
		t.Fatal("native header or terminators changed")
	}
	if !bytes.Equal(live, want) || !bytes.Equal(talk, want) {
		t.Fatal("PLAY and Talk Open must preserve the dynamic password and all three native credential fields")
	}
}
