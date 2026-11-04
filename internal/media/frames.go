package media

import (
	"bytes"
	"encoding/binary"
	"errors"
)

const maxCPacket = 8 * 1024 * 1024

type MediaFrame struct {
	Header [20]byte
	Data   []byte
}
type CPacketAssembler struct{ buffer []byte }

func (a *CPacketAssembler) Feed(data []byte) ([]MediaFrame, error) {
	var frames []MediaFrame
	for len(data) > 0 {
		needed := 20 - len(a.buffer)
		if needed <= 0 {
			size := int(binary.LittleEndian.Uint32(a.buffer[4:8]))
			if size <= 0 || size > maxCPacket-20 {
				return nil, errors.New("invalid CPacket payload length")
			}
			needed = 20 + size - len(a.buffer)
		}
		take := min(needed, len(data))
		a.buffer = append(a.buffer, data[:take]...)
		data = data[take:]
		if len(a.buffer) < 20 {
			continue
		}
		if !bytes.Equal(a.buffer[:3], []byte{0, 0, 1}) || a.buffer[3] < 0xe0 || a.buffer[3] > 0xea {
			return nil, errors.New("invalid CPacket magic")
		}
		size := int(binary.LittleEndian.Uint32(a.buffer[4:8]))
		if size <= 0 || size > maxCPacket-20 {
			return nil, errors.New("invalid CPacket payload length")
		}
		if len(a.buffer) == 20+size {
			var frame MediaFrame
			copy(frame.Header[:], a.buffer[:20])
			frame.Data = append([]byte(nil), a.buffer[20:]...)
			frames = append(frames, frame)
			a.buffer = nil
		}
	}
	return frames, nil
}
func (a *CPacketAssembler) Finish() error {
	if len(a.buffer) > 0 {
		return errors.New("truncated CPacket at capture end")
	}
	return nil
}

// Retain parameter sets before the first IDR, and omit undecodable early P frames.
type h264CaptureState struct {
	sps, pps []byte
	started  bool
	fps      byte
}

func (s *h264CaptureState) accept(data []byte) ([]byte, bool) {
	type nal struct {
		start, end int
		kind       byte
	}
	var nals []nal
	for i := 0; i+3 < len(data); {
		prefix := 0
		if i+4 <= len(data) && bytes.Equal(data[i:i+4], []byte{0, 0, 0, 1}) {
			prefix = 4
		} else if bytes.Equal(data[i:i+3], []byte{0, 0, 1}) {
			prefix = 3
		}
		if prefix == 0 {
			i++
			continue
		}
		if i+prefix >= len(data) {
			break
		}
		if len(nals) > 0 {
			nals[len(nals)-1].end = i
		}
		nals = append(nals, nal{i, len(data), data[i+prefix] & 31})
		i += prefix + 1
	}
	idr, picture := false, false
	for _, n := range nals {
		switch n.kind {
		case 7:
			s.sps = append([]byte(nil), data[n.start:n.end]...)
		case 8:
			s.pps = append([]byte(nil), data[n.start:n.end]...)
		case 5:
			idr = true
			picture = true
		case 1:
			picture = true
		}
	}
	if !s.started {
		if !idr || len(s.sps) == 0 || len(s.pps) == 0 {
			return nil, false
		}
		s.started = true
		out := append([]byte(nil), s.sps...)
		out = append(out, s.pps...)
		for _, n := range nals {
			if n.kind != 7 && n.kind != 8 {
				out = append(out, data[n.start:n.end]...)
			}
		}
		return out, picture
	}
	return data, picture
}
