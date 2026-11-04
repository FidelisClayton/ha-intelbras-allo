package media

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func cpacket(kind, codec byte, payload []byte) []byte {
	h := make([]byte, 20)
	copy(h, []byte{0, 0, 1, kind})
	binary.LittleEndian.PutUint32(h[4:8], uint32(len(payload)))
	h[14] = codec
	h[15] = 1
	if kind == 0xe3 {
		binary.LittleEndian.PutUint16(h[16:18], 8000)
	} else {
		binary.LittleEndian.PutUint16(h[16:18], 640)
		binary.LittleEndian.PutUint16(h[18:20], 480)
	}
	return append(h, payload...)
}
func TestCPacketFragmentsAndMultiple(t *testing.T) {
	data := append(cpacket(0xe1, 1, []byte{0, 0, 0, 1, 0x65}), cpacket(0xe3, 4, []byte{0xd5, 0x55})...)
	for split := 1; split < len(data); split++ {
		var a CPacketAssembler
		first, err := a.Feed(data[:split])
		if err != nil {
			t.Fatal(err)
		}
		second, err := a.Feed(data[split:])
		if err != nil {
			t.Fatal(err)
		}
		frames := append(first, second...)
		if len(frames) != 2 || frames[0].Header[3] != 0xe1 || frames[1].Header[3] != 0xe3 || a.Finish() != nil {
			t.Fatalf("split%d produced%d", split, len(frames))
		}
	}
}
func TestCPacketRejectsMalformedAndTruncated(t *testing.T) {
	for _, size := range []uint32{0, maxCPacket, ^uint32(0)} {
		data := cpacket(0xe1, 1, []byte{1})
		binary.LittleEndian.PutUint32(data[4:8], size)
		var a CPacketAssembler
		if _, err := a.Feed(data); err == nil {
			t.Fatal("bad size accepted")
		}
	}
	var a CPacketAssembler
	if _, err := a.Feed([]byte{0, 0}); err != nil {
		t.Fatal(err)
	}
	if a.Finish() == nil {
		t.Fatal("truncation lost")
	}
	var b CPacketAssembler
	if _, err := b.Feed(bytes.Repeat([]byte{0xff}, 20)); err == nil {
		t.Fatal("bad magic accepted")
	}
}
func TestH264WaitsForParameterSetsAndIDR(t *testing.T) {
	var state h264CaptureState
	for _, data := range [][]byte{{0, 0, 1, 0x41, 1}, {0, 0, 0, 1, 0x67, 2}, {0, 0, 1, 0x68, 3}} {
		if out, picture := state.accept(data); len(out) != 0 || picture {
			t.Fatal("output before complete initial keyframe")
		}
	}
	out, picture := state.accept([]byte{0, 0, 1, 0x65, 4})
	if !picture || !bytes.Equal(out, []byte{0, 0, 0, 1, 0x67, 2, 0, 0, 1, 0x68, 3, 0, 0, 1, 0x65, 4}) {
		t.Fatalf("parameter sets lost %x", out)
	}
	out, picture = state.accept([]byte{0, 0, 1, 0x41, 5})
	if !picture || len(out) == 0 {
		t.Fatal("subsequent P frame lost")
	}
}
