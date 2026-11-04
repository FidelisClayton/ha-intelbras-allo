package media

// QUII layouts follow the MIT-licensed quii-helper project (quii_helper/protocols).
// The fixed vendor CBC IV and unauthenticated media are protocol constraints.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const maxWireMedia = 8 * 1024 * 1024

type WirePacket struct {
	Type    byte
	Header  [32]byte
	Payload []byte
	IsMedia bool
}

type SetupReply struct {
	Header     [32]byte
	Status     byte
	CryptoMode byte
	SHAMode    byte
}

type WireCodec struct{ block cipher.Block }

func NewWireCodec(streamKey string) (*WireCodec, error) {
	if len(streamKey) != 32 {
		return nil, errors.New("stream key must be exactly 32 bytes")
	}
	block, err := aes.NewCipher([]byte(streamKey))
	if err != nil {
		return nil, err
	}
	return &WireCodec{block: block}, nil
}

func BuildSetup(mode byte) []byte {
	header := make([]byte, 32)
	header[0], header[9] = 0xa9, mode
	return header
}

func ParseSetup(reader io.Reader) (SetupReply, error) {
	var reply SetupReply
	if _, err := io.ReadFull(reader, reply.Header[:]); err != nil {
		return reply, fmt.Errorf("read setup: %w", err)
	}
	reply.Status, reply.CryptoMode, reply.SHAMode = reply.Header[9], reply.Header[10], reply.Header[11]
	if reply.Header[0] != 0xa9 {
		return reply, errors.New("invalid setup reply type")
	}
	if reply.Status != 0 {
		return reply, fmt.Errorf("setup rejected with status %d", reply.Status)
	}
	if reply.CryptoMode != 2 || reply.SHAMode != 1 {
		return reply, errors.New("setup requires AES-256/SHA256")
	}
	return reply, nil
}

func (c *WireCodec) crypt(data []byte, decrypt bool) []byte {
	result := make([]byte, len(data))
	iv := []byte("0000000000000000")
	if decrypt {
		cipher.NewCBCDecrypter(c.block, iv).CryptBlocks(result, data)
	} else {
		cipher.NewCBCEncrypter(c.block, iv).CryptBlocks(result, data)
	}
	return result
}

// EncodeCommand leaves raw-length, channel and parameter fields to the caller:
// talk request channels overlap the live command raw-length field.
func (c *WireCodec) EncodeCommand(header [32]byte, body []byte) ([]byte, error) {
	if len(body) > 65520-sha256.Size {
		return nil, errors.New("command body too large")
	}
	size := (len(body) + sha256.Size + 15) / 16 * 16
	binary.LittleEndian.PutUint16(header[9:11], uint16(size))
	digest := sha256.New()
	digest.Write(header[:])
	digest.Write(body)
	plain := make([]byte, size)
	copy(plain, body)
	copy(plain[len(body):], digest.Sum(nil))
	return append(c.crypt(header[:], false), c.crypt(plain, false)...), nil
}

// BuildMedia encrypts only the declared prefix; media offsets remain caller-owned.
func (c *WireCodec) BuildMedia(header [32]byte, payload []byte, prefix int) ([]byte, error) {
	if !wireMediaType(header[0]) || len(payload) == 0 || len(payload) > maxWireMedia {
		return nil, errors.New("invalid media type or length")
	}
	if prefix < 0 || prefix > 65535 || prefix > len(payload) || prefix%16 != 0 {
		return nil, errors.New("invalid encrypted media prefix")
	}
	if header[15] != 0 || int(binary.LittleEndian.Uint16(header[16:18])) > len(payload) {
		return nil, errors.New("unsupported media encryption or invalid offset")
	}
	binary.LittleEndian.PutUint16(header[9:11], uint16(prefix))
	binary.LittleEndian.PutUint32(header[11:15], uint32(len(payload)))
	packet := c.crypt(header[:], false)
	packet = append(packet, c.crypt(payload[:prefix], false)...)
	return append(packet, payload[prefix:]...), nil
}

func wireMediaType(kind byte) bool { return kind >= 0xa0 && kind <= 0xa3 }

// ReadPacket preserves command padding. Sessions validate reply-specific SHA256:
// command bytes 11 onward may contain status/channel rather than a body length.
// A read error is terminal for this reader; no partial-packet state is retained.
func (c *WireCodec) ReadPacket(reader io.Reader) (WirePacket, error) {
	var packet WirePacket
	var encrypted [32]byte
	if _, err := io.ReadFull(reader, encrypted[:]); err != nil {
		return packet, fmt.Errorf("read packet header: %w", err)
	}
	copy(packet.Header[:], c.crypt(encrypted[:], true))
	header := packet.Header[:]
	packet.Type, packet.IsMedia = header[0], wireMediaType(header[0])
	prefix := int(binary.LittleEndian.Uint16(header[9:11]))
	size, offset := prefix, 0
	if packet.IsMedia {
		mediaSize := binary.LittleEndian.Uint32(header[11:15])
		if mediaSize == 0 || mediaSize > maxWireMedia {
			return packet, errors.New("invalid media payload length")
		}
		size = int(mediaSize)
		offset = int(binary.LittleEndian.Uint16(header[16:18]))
		if header[15] != 0 || prefix%16 != 0 || prefix > size || offset > size {
			return packet, errors.New("unsupported media encryption or invalid prefix/offset")
		}
	} else if size < sha256.Size || size%16 != 0 {
		return packet, errors.New("invalid encrypted command length")
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return packet, fmt.Errorf("read packet payload: %w", err)
	}
	copy(payload[:prefix], c.crypt(payload[:prefix], true))
	packet.Payload = payload[offset:]
	return packet, nil
}
