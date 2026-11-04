package discovery

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net/netip"
	"testing"
	"testing/synctest"
	"time"
)

// Independently generated with Python cryptography and the native lookup data.
const syntheticCiphertext = "ab73031b0498ab392015273656ff721a130ebb3c3a0101b005e67d48b59aeb6066961760fc24aa90707acc9543cafc4bec16436299ebeb30eb2c68ea554c7db68ec4b00aa11a46647f01880b71e9700588fad403f3d3fa44b041547fd2f30b2d2506c365b9d36140af45c2a207362d0b36beb2d7e7d33a34c104cbc89d6289cf8d016a7c189137bed03d9d34b4080d2526b4ac326bb7beaf4482dfe0e2865fc0cb64a12605dbe61d452f694490820b59219f56b617e52da50e98df8ce9bd3e5b77a3f3e9f05c25c73f34d651ccf888b9d523994bebb9daf1fa1b87044f3a7a524512f9b36e43219135f4564368af9dad872f9ef7ac4ad520692a24019fbad4cb38caff6ae90ec7d252b4587b2d4a3ed7aea3c9712afdceb7a75e8fc6a00003b63ad65b8b8756f7fbe3bdd27852bb3a1bfb8df6ce437497f1ec75bc199904cce75a7499bdabffce79db21e9ce8618f71f8a0334bfb426f2aea32c83e0c876e1cbc66e53185f412937e484a5efeecc851c3ac3411229f882eaa35bb158cc44f53ac714c5a5d15f5fd8bd3718c7db5a9074662bbc64c81ad8ed84f50e843459976c8a639b2032c3e1df37748103504aa8e70baa5b1f4e9b5ba45b9ebd56fb0c62d165e31e10d62e5feb143c3a03852c4f18c6640deadb0d543eed9dbe54b46475147311dfd471edb1d7fdd1db11097c9667e5e9e5a7d44f34ad84876213fa3f761d04cd4267b69bcb1a312796dca99169b4"

func syntheticPacket(t *testing.T) []byte {
	t.Helper()
	ciphertext, err := hex.DecodeString(syntheticCiphertext)
	if err != nil {
		t.Fatal(err)
	}
	packet := make([]byte, 56+32+len(ciphertext))
	copy(packet, "ASZENO.SEARCH.V4")
	binary.LittleEndian.PutUint32(packet[24:], 32)
	binary.LittleEndian.PutUint32(packet[28:], uint32(len(ciphertext)))
	for i := 0; i < 32; i++ {
		packet[56+i] = byte(i)
	}
	copy(packet[88:], ciphertext)
	return packet
}

func TestNativeKeyKnownAnswer(t *testing.T) {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i)
	}
	got := deriveKey(seed)
	want, _ := hex.DecodeString("4498a865b938d2e488576735c5f635cc2e9160703eceff3e1a3d0e20b04cdd2d")
	if !bytes.Equal(got[:], want) {
		t.Fatalf("derived key mismatch: %x", got)
	}
}

func TestDecodeReply(t *testing.T) {
	p := syntheticPacket(t)
	ip := netip.MustParseAddr("192.168.4.20")
	got, err := decodeReply(p, ip, "TESTDEVICE01")
	if err != nil || got != ip {
		t.Fatalf("synthetic reply: %v %v", got, err)
	}
	if _, err := decodeReply(p, ip, "OTHERDEVICE"); !errors.Is(err, errOtherUID) {
		t.Fatal("must ignore another UID")
	}
	for _, ip := range []string{"8.8.8.8", "127.0.0.1", "224.0.0.1", "::1", "fc00::1"} {
		if _, err := decodeReply(p, netip.MustParseAddr(ip), "TESTDEVICE01"); !errors.Is(err, ErrDecode) {
			t.Fatal("accepted non-private IPv4 source")
		}
	}
}

func TestMalformedReplies(t *testing.T) {
	cases := map[string]func([]byte) []byte{
		"short":           func(p []byte) []byte { return p[:55] },
		"prefix":          func(p []byte) []byte { p[0] = 'X'; return p },
		"seed":            func(p []byte) []byte { binary.LittleEndian.PutUint32(p[24:], 33); return p },
		"huge":            func(p []byte) []byte { binary.LittleEndian.PutUint32(p[28:], 0xffffffff); return p },
		"block":           func(p []byte) []byte { binary.LittleEndian.PutUint32(p[28:], 527); return p },
		"truncated":       func(p []byte) []byte { return p[:len(p)-1] },
		"short plaintext": func(p []byte) []byte { binary.LittleEndian.PutUint32(p[28:], 512); return p[:88+512] },
		"oversized":       func(p []byte) []byte { return append(p, make([]byte, 65508)...) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := decodeReply(mutate(syntheticPacket(t)), netip.MustParseAddr("192.168.4.20"), "TESTDEVICE01")
			if !errors.Is(err, ErrDecode) {
				t.Fatalf("accepted malformed packet: %v", err)
			}
			if err.Error() != ErrDecode.Error() {
				t.Fatal("decode error leaked packet data")
			}
		})
	}
}

type fakePacket struct {
	data []byte
	ip   netip.Addr
}
type fakeTransport struct {
	packets []fakePacket
	sent    int
	closed  bool
}

func (f *fakeTransport) send() error { f.sent++; return nil }
func (f *fakeTransport) receive(ctx context.Context) ([]byte, netip.Addr, error) {
	if len(f.packets) > 0 {
		p := f.packets[0]
		f.packets = f.packets[1:]
		return p.data, p.ip, nil
	}
	<-ctx.Done()
	return nil, netip.Addr{}, ctx.Err()
}
func (f *fakeTransport) close() { f.closed = true }
func TestDiscoverPacketBoundary(t *testing.T) {
	f := &fakeTransport{packets: []fakePacket{{syntheticPacket(t), netip.MustParseAddr("192.168.4.20")}}}
	got, err := discover(context.Background(), "TESTDEVICE01", func(context.Context) (transport, error) { return f, nil })
	if err != nil || got != netip.MustParseAddr("192.168.4.20") || f.sent != 1 || !f.closed {
		t.Fatal("single targeted discovery failed", err)
	}
}
func TestDiscoverCancellationAndPrivacy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := discover(ctx, "TESTDEVICE01", func(context.Context) (transport, error) { t.Fatal("opened after cancellation"); return nil, nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	f := &fakeTransport{packets: []fakePacket{{[]byte("private packet"), netip.MustParseAddr("192.168.4.20")}}}
	_, err = discover(ctx, "TESTDEVICE01", func(context.Context) (transport, error) { return f, nil })
	if !errors.Is(err, ErrDecode) || err.Error() != ErrDecode.Error() {
		t.Fatal("expected fixed decode error", err)
	}
	if f.sent != 1 || !f.closed {
		t.Fatal("query/close boundary")
	}
}

func TestDiscoverIgnoresOtherUID(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	f := &fakeTransport{packets: []fakePacket{{syntheticPacket(t), netip.MustParseAddr("192.168.4.20")}}}
	_, err := discover(ctx, "ANOTHERDEVICE", func(context.Context) (transport, error) { return f, nil })
	if !errors.Is(err, ErrTimeout) {
		t.Fatal("another UID must not be returned", err)
	}
}

func TestDiscoverPortAndUIDErrors(t *testing.T) {
	_, err := discover(context.Background(), "TESTDEVICE01", func(context.Context) (transport, error) { return nil, ErrPortInUse })
	if !errors.Is(err, ErrPortInUse) {
		t.Fatal(err)
	}
	for _, uid := range []string{"", "bad/uid", string(bytes.Repeat([]byte{'A'}, 64))} {
		_, err := discover(context.Background(), uid, func(context.Context) (transport, error) { t.Fatal("opened with invalid UID"); return nil, nil })
		if !errors.Is(err, ErrUID) {
			t.Fatal(err)
		}
	}
}

func TestDiscoverWaitingForSlotIsCancellable(t *testing.T) {
	discoverySlot <- struct{}{}
	defer func() { <-discoverySlot }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	_, err := discover(ctx, "TESTDEVICE01", func(context.Context) (transport, error) { t.Fatal("opened while slot occupied"); return nil, nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func TestReplyUIDValidation(t *testing.T) {
	key, _ := hex.DecodeString("4498a865b938d2e488576735c5f635cc2e9160703eceff3e1a3d0e20b04cdd2d")
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range [][]byte{nil, []byte("BAD/UID"), bytes.Repeat([]byte{'A'}, 64), []byte{0xff}} {
		packet := syntheticPacket(t)
		plaintext := make([]byte, 528)
		copy(plaintext[200:264], field)
		cipher.NewCBCEncrypter(block, []byte("0000000000000000")).CryptBlocks(packet[88:], plaintext)
		if _, err := decodeReply(packet, netip.MustParseAddr("192.168.4.20"), "TESTDEVICE01"); !errors.Is(err, ErrDecode) {
			t.Fatal("accepted invalid UID", err)
		}
	}
}

type droppedQueryTransport struct {
	fakeTransport
	sentAt []time.Time
}

func (f *droppedQueryTransport) send() error {
	f.sentAt = append(f.sentAt, time.Now())
	return f.fakeTransport.send()
}
func (f *droppedQueryTransport) receive(ctx context.Context) ([]byte, netip.Addr, error) {
	if f.sent < 2 {
		<-ctx.Done()
		return nil, netip.Addr{}, ctx.Err()
	}
	return f.fakeTransport.receive(ctx)
}
func TestDiscoverRepeatsDroppedQuery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
		defer cancel()
		f := &droppedQueryTransport{fakeTransport: fakeTransport{packets: []fakePacket{{syntheticPacket(t), netip.MustParseAddr("192.168.4.20")}}}}
		started := time.Now()
		got, err := discover(ctx, "TESTDEVICE01", func(context.Context) (transport, error) { return f, nil })
		if err != nil || got != netip.MustParseAddr("192.168.4.20") {
			t.Fatal("must recover from dropped initial query", err)
		}
		if f.sent != 2 || !f.closed {
			t.Fatalf("expected exactly two sends then close; got %d sends", f.sent)
		}
		if !f.sentAt[0].Equal(started) || time.Since(started) != time.Second {
			t.Fatal("initial query or completion was delayed")
		}
		if f.sentAt[1].Sub(f.sentAt[0]) != time.Second {
			t.Fatal("query repeated before one second")
		}
	})
}

func TestDiscoverRepeatsWithinDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeTransport{}
		started := time.Now()
		_, err := discover(context.Background(), "TESTDEVICE01", func(context.Context) (transport, error) { return f, nil })
		if !errors.Is(err, ErrTimeout) || f.sent != 5 || !f.closed || time.Since(started) != 5*time.Second {
			t.Fatalf("five-second discovery bound: sends=%d, elapsed=%v, err=%v", f.sent, time.Since(started), err)
		}
	})
}

// A deadline can pass before its cancellation has propagated to child contexts.
type pendingDeadlineContext struct {
	context.Context
	deadline time.Time
}

func (c pendingDeadlineContext) Deadline() (time.Time, bool) { return c.deadline, true }

type deadlineTransport struct {
	fakeTransport
	deadline time.Time
}

func (f *deadlineTransport) receive(context.Context) ([]byte, netip.Addr, error) {
	time.Sleep(time.Until(f.deadline))
	return nil, netip.Addr{}, context.DeadlineExceeded
}

func TestDiscoverDoesNotSendAfterDeadlineBeforeCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		deadline := time.Now().Add(4 * time.Second)
		ctx := pendingDeadlineContext{Context: context.Background(), deadline: deadline}
		f := &deadlineTransport{deadline: deadline}
		_, err := discover(ctx, "TESTDEVICE01", func(context.Context) (transport, error) { return f, nil })
		if !errors.Is(err, ErrTimeout) || f.sent != 1 || !f.closed || !time.Now().Equal(deadline) {
			t.Fatalf("expired discovery with pending cancellation: sends=%d, err=%v", f.sent, err)
		}
	})
}
