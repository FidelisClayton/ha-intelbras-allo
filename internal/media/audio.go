package media

import (
	"encoding/binary"
	"errors"
)

func DecodeALaw(v byte) int16 {
	v ^= 0x55
	sample := int(v&15) << 4
	segment := (v & 0x70) >> 4
	switch segment {
	case 0:
		sample += 8
	case 1:
		sample += 0x108
	default:
		sample = (sample + 0x108) << (segment - 1)
	}
	if v&0x80 == 0 {
		sample = -sample
	}
	return int16(sample)
}
func DecodeMuLaw(v byte) int16 {
	v = ^v
	sample := ((int(v&15) << 3) + 0x84) << ((v & 0x70) >> 4)
	sample -= 0x84
	if v&0x80 != 0 {
		sample = -sample
	}
	return int16(sample)
}
func decodeAudio(codec byte, data []byte) ([]byte, error) {
	if codec == 9 {
		if len(data)%2 != 0 {
			return nil, errors.New("odd PCM16 payload length")
		}
		return data, nil
	}
	if codec != 4 && codec != 5 {
		return nil, errors.New("unsupported audio codec")
	}
	pcm := make([]byte, len(data)*2)
	for i, v := range data {
		sample := DecodeALaw(v)
		if codec == 5 {
			sample = DecodeMuLaw(v)
		}
		binary.LittleEndian.PutUint16(pcm[2*i:], uint16(sample))
	}
	return pcm, nil
}

// EncodeMuLaw encodes PCM16 samples for an 8 kHz mono PCMU track.
func EncodeMuLaw(pcm []int16) []byte { return encodeG711(pcm, 5) }
