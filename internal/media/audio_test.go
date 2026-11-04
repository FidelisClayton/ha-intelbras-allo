package media

import (
	"bytes"
	"testing"
)

func TestG711KnownVectors(t *testing.T) {
	for _, tt := range []struct {
		in          byte
		alaw, mulaw int16
	}{{0xd5, 8, 716}, {0x55, -8, -716}, {0xff, 848, 0}, {0x7f, -848, 0}, {0, -5504, -32124}, {0x80, 5504, 32124}} {
		if got := DecodeALaw(tt.in); got != tt.alaw {
			t.Errorf("ALaw %x=%d want%d", tt.in, got, tt.alaw)
		}
		if got := DecodeMuLaw(tt.in); got != tt.mulaw {
			t.Errorf("MuLaw %x=%d want%d", tt.in, got, tt.mulaw)
		}
	}
	pcm, err := decodeAudio(4, []byte{0xd5, 0x55})
	if err != nil || !bytes.Equal(pcm, []byte{8, 0, 0xf8, 0xff}) {
		t.Fatalf("PCM conversion %x %v", pcm, err)
	}
	if _, err = decodeAudio(7, []byte{1}); err == nil {
		t.Fatal("unsupported audio accepted")
	}
	if _, err = decodeAudio(9, []byte{1}); err == nil {
		t.Fatal("odd PCM accepted")
	}
}
