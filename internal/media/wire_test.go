package media

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"testing"
)

const wireTestKey = "0123456789abcdef0123456789abcdef"

func testWireCodec(t *testing.T) *WireCodec {
	t.Helper()
	c, err := NewWireCodec(wireTestKey)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestWireKnownAnswer(t *testing.T) {
	c := testWireCodec(t)
	// OpenSSL enc -aes-256-cbc -nopad, ASCII test key and ASCII-zero IV.
	want, _ := hex.DecodeString("5e15d51bebe1a68a197988006e142e6e5f4289384ca532df2dc0f2f6cb7bfafa")
	if got := c.crypt(make([]byte, 32), false); !bytes.Equal(got, want) {
		t.Fatalf("known answer: %x", got)
	}
	if got := c.crypt(want, true); !bytes.Equal(got, make([]byte, 32)) {
		t.Fatal("decrypt known answer")
	}
	for _, key := range []string{"", wireTestKey[:31], wireTestKey + "0", "ééééééééééééééééé"} {
		if _, err := NewWireCodec(key); err == nil {
			t.Fatal("accepted invalid key length")
		}
	}
}

func TestWireCommandRoundTrip(t *testing.T) {
	c := testWireCodec(t)
	var header [32]byte
	header[0], header[11], header[12], header[13] = 0x0c, 0xff, 0xff, 1
	body := []byte("example")
	wire, err := c.EncodeCommand(header, body)
	if err != nil {
		t.Fatal(err)
	}
	packet, err := c.ReadPacket(&oneByteReader{bytes.NewReader(wire)})
	if err != nil {
		t.Fatal(err)
	}
	if packet.IsMedia || packet.Type != 0x0c || packet.Header[11] != 0xff || packet.Header[12] != 0xff || packet.Header[13] != 1 {
		t.Fatal("command header changed")
	}
	if len(packet.Payload) != 48 || binary.LittleEndian.Uint16(packet.Header[9:11]) != 48 || !bytes.Equal(packet.Payload[:len(body)], body) {
		t.Fatal("incorrect command lengths/body")
	}
	digest := sha256.Sum256(append(packet.Header[:], body...))
	if !bytes.Equal(packet.Payload[len(body):len(body)+32], digest[:]) {
		t.Fatal("incorrect command SHA256")
	}
	if !bytes.Equal(packet.Payload[len(body)+32:], make([]byte, 9)) {
		t.Fatal("incorrect zero padding")
	}
	if _, err := c.EncodeCommand(header, make([]byte, 65520-32+1)); err == nil {
		t.Fatal("accepted oversized command")
	}
}

type oneByteReader struct{ *bytes.Reader }

func (r *oneByteReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return r.Reader.Read(p)
}

func TestWireMediaRoundTrip(t *testing.T) {
	c := testWireCodec(t)
	for _, kind := range []byte{0xa0, 0xa1, 0xa2, 0xa3} {
		for _, prefix := range []int{0, 16, 32} {
			var header [32]byte
			header[0] = kind
			binary.LittleEndian.PutUint16(header[16:18], 7)
			payload := bytes.Repeat([]byte{1, 2, 3}, 23)
			wire, err := c.BuildMedia(header, payload, prefix)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(wire[32+prefix:], payload[prefix:]) {
				t.Fatal("plaintext tail changed")
			}
			packet, err := c.ReadPacket(&oneByteReader{bytes.NewReader(wire)})
			if err != nil {
				t.Fatal(err)
			}
			if !packet.IsMedia || packet.Type != kind || !bytes.Equal(packet.Payload, payload[7:]) {
				t.Fatal("media round trip failed")
			}
		}
	}
}

func TestWireMalformedPackets(t *testing.T) {
	c := testWireCodec(t)
	for _, tc := range []struct {
		name           string
		kind           byte
		size           uint32
		prefix, offset uint16
		flag           byte
	}{
		{"oversized", 0xa0, maxWireMedia + 1, 0, 0, 0},
		{"overflow", 0xa0, 0xffffffff, 0, 0, 0},
		{"empty", 0xa0, 0, 0, 0, 0},
		{"prefix exceeds payload", 0xa0, 17, 32, 0, 0},
		{"unaligned prefix", 0xa2, 17, 1, 0, 0},
		{"offset exceeds payload", 0xa1, 17, 16, 18, 0},
		{"separate encryption", 0xa3, 32, 16, 0, 1},
		{"command short", 1, 0, 16, 0, 0},
		{"command unaligned", 1, 0, 33, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var header [32]byte
			header[0], header[15] = tc.kind, tc.flag
			binary.LittleEndian.PutUint16(header[9:11], tc.prefix)
			binary.LittleEndian.PutUint32(header[11:15], tc.size)
			binary.LittleEndian.PutUint16(header[16:18], tc.offset)
			if _, err := c.ReadPacket(bytes.NewReader(c.crypt(header[:], false))); err == nil {
				t.Fatal("accepted malformed header")
			}
		})
	}
	var header [32]byte
	header[0] = 0xa2
	for _, prefix := range []int{-1, 1, 48, 65536} {
		if _, err := c.BuildMedia(header, make([]byte, 32), prefix); err == nil {
			t.Fatal("accepted invalid prefix")
		}
	}
	header[16] = 33
	if _, err := c.BuildMedia(header, make([]byte, 32), 16); err == nil {
		t.Fatal("accepted invalid offset")
	}
	header[16] = 0
	wire, err := c.BuildMedia(header, make([]byte, 32), 16)
	if err != nil {
		t.Fatal(err)
	}
	for cut := 0; cut < len(wire); cut++ {
		if _, err := c.ReadPacket(bytes.NewReader(wire[:cut])); err == nil {
			t.Fatalf("accepted truncation at %d", cut)
		}
	}
	wrong, _ := NewWireCodec("fedcba9876543210fedcba9876543210")
	if _, err := wrong.ReadPacket(bytes.NewReader(wire)); err == nil {
		t.Fatal("wrong-key fixture accepted")
	}
	// Media has no MAC: wrong-key rejection above is structural, not authentication.
}

func TestWireSetup(t *testing.T) {
	for _, mode := range []byte{0, 2} {
		setup := BuildSetup(mode)
		if len(setup) != 32 || setup[0] != 0xa9 || setup[9] != mode {
			t.Fatal("incorrect setup")
		}
	}
	good := make([]byte, 32)
	good[0], good[10], good[11] = 0xa9, 2, 1
	reply, err := ParseSetup(&oneByteReader{bytes.NewReader(good)})
	if err != nil || reply.Status != 0 || reply.CryptoMode != 2 || reply.SHAMode != 1 {
		t.Fatal("setup parse failed", err)
	}
	for _, index := range []int{0, 9, 10, 11} {
		bad := bytes.Clone(good)
		bad[index]++
		if _, err := ParseSetup(bytes.NewReader(bad)); err == nil {
			t.Fatalf("accepted bad setup byte %d", index)
		}
	}
	if _, err := ParseSetup(io.LimitReader(bytes.NewReader(good), 31)); err == nil {
		t.Fatal("accepted short setup")
	}
}
