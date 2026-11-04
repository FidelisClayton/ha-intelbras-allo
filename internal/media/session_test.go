package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestOpaqueCredentialBodyAndTarget(t *testing.T) {
	creds := testCredentials{body: []byte("sdk&&opaque&+/==\x00\x00\x00")}
	_, live := livePlayCommand(creds, 1, 2, 1)
	_, talk := talkOpen(creds, 65535, time.Now())
	if !bytes.Equal(live, creds.body) || !bytes.Equal(talk, creds.body) {
		t.Fatal("opaque body changed")
	}
	for _, target := range []Target{{"same", "example.com"}, {"same", "8.8.8.8"}, {"other", "192.168.1.2"}} {
		if validateLANTarget(target, creds) == nil {
			t.Fatal("unsafe target accepted")
		}
	}
	if err := validateLANTarget(Target{"same", "192.168.1.2"}, creds); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeFrames(t *testing.T) {
	var state h264CaptureState
	video := cpacket(0xe1, 1, []byte{0, 0, 1, 0x67, 2, 0, 0, 1, 0x68, 3, 0, 0, 1, 0x65, 4})
	video[15] = 100
	audio := cpacket(0xe3, 5, []byte{0xff, 0x80})
	var assembler CPacketAssembler
	packets, err := assembler.Feed(append(video, audio...))
	if err != nil {
		t.Fatal(err)
	}
	f, err := decodeFrame(packets[0], &state)
	if err != nil || f.Codec != CodecH264 || f.Duration != 40*time.Millisecond || len(f.Data) == 0 {
		t.Fatalf("video %+v %v", f, err)
	}
	f, err = decodeFrame(packets[1], &state)
	if err != nil || f.Codec != CodecPCM16 || f.Duration != 250*time.Microsecond || binary.LittleEndian.Uint16(f.Data[2:]) != 32124 {
		t.Fatalf("audio %+v %v", f, err)
	}
	packets[1].Header[16], packets[1].Header[17] = 0, 0
	if _, err = decodeFrame(packets[1], &state); err == nil {
		t.Fatal("invalid sample rate accepted")
	}
}

func TestTalkStreamingPacesAcrossCallsAndClose(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	c := testCodec(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stamps := make(chan time.Time, 2)
	done := make(chan error, 1)
	go func() {
		if err := fakeTalkHandshake(c, server, 0, 2, 1); err != nil {
			done <- err
			return
		}
		for i := 0; i < 2; i++ {
			p, err := c.ReadPacket(server)
			if err != nil {
				done <- err
				return
			}
			if p.Type != 0xa2 || len(p.Payload) != 340 || p.Payload[14] != 5 {
				done <- errors.New("invalid microphone packet")
				return
			}
			stamps <- time.Now()
		}
		var b [1]byte
		_, err := server.Read(b[:])
		if !errors.Is(err, io.EOF) {
			done <- errors.New("talk did not release connection")
			return
		}
		done <- nil
	}()
	s, err := openTalkConn(ctx, client, c, syntheticMediaCredentials(), 0, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.WritePCM(make([]int16, 159)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stamps:
		t.Fatal("partial packet sent")
	default:
	}
	if err = s.WritePCM(make([]int16, 161)); err != nil {
		t.Fatal(err)
	}
	if err = s.WritePCM(make([]int16, 320)); err != nil {
		t.Fatal(err)
	}
	first, second := <-stamps, <-stamps
	if second.Sub(first) < 35*time.Millisecond {
		t.Fatal("separate writes burst")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if s.WritePCM(make([]int16, 320)) == nil {
		t.Fatal("sent after release")
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSessionOwnsTalkAndClosesOnCancellation(t *testing.T) {
	c := testCodec(t)
	liveClient, liveServer := net.Pipe()
	defer liveServer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	live := &liveSession{conn: liveClient, codec: c, ctx: ctx, cancel: cancel, ready: make(chan struct{})}
	live.stop = context.AfterFunc(ctx, func() { _ = liveClient.Close() })
	close(live.ready)
	s := startSession(live, Target{}, syntheticMediaCredentials())
	talkClient, talkServer := net.Pipe()
	defer talkServer.Close()
	done := make(chan error, 1)
	go func() {
		if err := fakeTalkHandshake(c, talkServer, 0, 1, 0); err != nil {
			done <- err
			return
		}
		var b [1]byte
		_, err := talkServer.Read(b[:])
		if !errors.Is(err, io.EOF) {
			done <- errors.New("talk left open")
			return
		}
		done <- nil
	}()
	talk, err := s.openTalk(context.Background(), func(ctx context.Context) (*TalkSession, error) {
		return openTalkConn(ctx, talkClient, c, syntheticMediaCredentials(), 0, time.Hour)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.OpenTalk(context.Background()); err == nil {
		t.Fatal("second owner accepted")
	}
	cancel()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-talk.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("talk context leaked")
	}
	if talk.WritePCM(make([]int16, 320)) == nil {
		t.Fatal("sent after session cancellation")
	}
	if _, err = s.ReadFrame(context.Background()); err == nil {
		t.Fatal("closed receiver succeeded")
	}
}

func TestReceiveDrainsWithSlowConsumer(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	c := testCodec(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	live := &liveSession{conn: client, codec: c, ctx: ctx, cancel: cancel, ready: make(chan struct{})}
	s := startSession(live, Target{}, syntheticMediaCredentials())
	defer s.Close()
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 100; i++ {
			h := liveHeader(0xa0)
			packet, _ := c.BuildMedia(h, cpacket(0xe3, 5, []byte{0xff}), 0)
			if err := writeFull(server, packet); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("receive blocked on slow consumer")
	}
	if len(s.frames) > 32 {
		t.Fatal("unbounded queue")
	}
	readCtx, stop := context.WithCancel(context.Background())
	stop()
	// A canceled caller must never stall the shared receive connection.
	_, _ = s.ReadFrame(readCtx)
}

func TestEncodeMuLawVectors(t *testing.T) {
	got := EncodeMuLaw([]int16{0, 32124, -32124})
	if !bytes.Equal(got, []byte{0xff, 0x80, 0}) {
		t.Fatalf("mu-law %x", got)
	}
}

func TestTalkWaitsForAcceptedReceive(t *testing.T) {
	liveCtx, stop := context.WithCancel(context.Background())
	defer stop()
	s := &Session{live: &liveSession{ctx: liveCtx, ready: make(chan struct{})}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	opened := false
	_, err := s.openTalk(ctx, func(context.Context) (*TalkSession, error) { opened = true; return nil, errors.New("unexpected dial") })
	if !errors.Is(err, context.Canceled) || opened {
		t.Fatal("talk opened before live acceptance")
	}
	s.talkMu.Lock()
	reserved := s.talking
	s.talkMu.Unlock()
	if reserved {
		t.Fatal("failed negotiation retained ownership")
	}
}

func TestTalkCloseReleasesOwner(t *testing.T) {
	c := testCodec(t)
	liveCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	live := &liveSession{ctx: liveCtx, ready: make(chan struct{})}
	close(live.ready)
	s := &Session{live: live}
	for i := 0; i < 2; i++ {
		client, server := net.Pipe()
		done := make(chan error, 1)
		go func() {
			defer server.Close()
			if err := fakeTalkHandshake(c, server, 0, 1, 0); err != nil {
				done <- err
				return
			}
			var b [1]byte
			_, err := server.Read(b[:])
			if !errors.Is(err, io.EOF) {
				done <- errors.New("transmitter left open")
				return
			}
			done <- nil
		}()
		talk, err := s.openTalk(liveCtx, func(ctx context.Context) (*TalkSession, error) {
			return openTalkConn(ctx, client, c, syntheticMediaCredentials(), 0, time.Hour)
		})
		if err != nil {
			t.Fatal(err)
		}
		if err = talk.Close(); err != nil {
			t.Fatal(err)
		}
		if err = <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestVideoUsesAdvertisedFPSForFollowingFrames(t *testing.T) {
	key := cpacket(0xe1, 1, []byte{0, 0, 1, 0x67, 2, 0, 0, 1, 0x68, 3, 0, 0, 1, 0x65, 4})
	key[15] = 80 // Native quarter-FPS field: 20 frames per second.
	delta := cpacket(0xe0, 1, []byte{0, 0, 1, 0x41, 4})
	delta[15] = 0
	var assembler CPacketAssembler
	packets, err := assembler.Feed(append(key, delta...))
	if err != nil {
		t.Fatal(err)
	}
	var state h264CaptureState
	for _, packet := range packets {
		frame, err := decodeFrame(packet, &state)
		if err != nil || frame.Duration != 50*time.Millisecond {
			t.Fatalf("lost advertised FPS: %v %+v", err, frame)
		}
	}
	var unknown h264CaptureState
	packets[0].Header[15] = 0
	if _, err = decodeFrame(packets[0], &unknown); err == nil {
		t.Fatal("missing native timing accepted")
	}
}
