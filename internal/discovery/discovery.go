// Package discovery locates only the LAN device matching a selected cloud UID.
package discovery

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"syscall"
	"time"
)

var (
	ErrDecode     = errors.New("discovery reply could not be decoded")
	ErrTimeout    = errors.New("discovery timed out")
	ErrPortInUse  = errors.New("discovery UDP port 5001 or 5003 is already in use")
	ErrNetwork    = errors.New("discovery UDP operation failed")
	ErrUID        = errors.New("discovery requires a valid selected device UID")
	errOtherUID   = errors.New("discovery reply belongs to another device")
	discoverySlot = make(chan struct{}, 1)
)

// Discover broadcasts the native query at 1 Hz and returns only the selected device's
// private IPv4 source address. The complete operation is bounded to five seconds.
func Discover(ctx context.Context, expectedUID string) (netip.Addr, error) {
	return discover(ctx, expectedUID, openUDP)
}

type transport interface {
	send() error
	receive(context.Context) ([]byte, netip.Addr, error)
	close()
}

func discover(ctx context.Context, uid string, open func(context.Context) (transport, error)) (netip.Addr, error) {
	if !validUID([]byte(uid)) {
		return netip.Addr{}, ErrUID
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	if err := ctx.Err(); err != nil {
		return netip.Addr{}, err
	}
	select {
	case discoverySlot <- struct{}{}:
		defer func() { <-discoverySlot }()
	case <-ctx.Done():
		return netip.Addr{}, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return netip.Addr{}, err
	}
	conn, err := open(ctx)
	if err != nil {
		return netip.Addr{}, err
	}
	defer conn.close()
	if err = conn.send(); err != nil {
		return netip.Addr{}, ErrNetwork
	}
	malformed := false
	nextQuery := time.Now().Add(time.Second)
	for {
		receiveCtx, cancelReceive := context.WithDeadline(ctx, nextQuery)
		packet, source, err := conn.receive(receiveCtx)
		queryDue := errors.Is(receiveCtx.Err(), context.DeadlineExceeded)
		cancelReceive()
		if err != nil {
			if errors.Is(ctx.Err(), context.Canceled) {
				return netip.Addr{}, context.Canceled
			}
			if ctx.Err() != nil || !time.Now().Before(deadline) {
				if malformed {
					return netip.Addr{}, ErrDecode
				}
				return netip.Addr{}, ErrTimeout
			}
			if queryDue {
				if err := conn.send(); err != nil {
					return netip.Addr{}, ErrNetwork
				}
				nextQuery = time.Now().Add(time.Second)
				continue
			}
			return netip.Addr{}, ErrNetwork
		}
		ip, err := decodeReply(packet, source, uid)
		if err == nil {
			return ip, nil
		}
		if errors.Is(err, ErrDecode) {
			malformed = true
		}
	}
}

func validUID(uid []byte) bool {
	if len(uid) == 0 || len(uid) > 63 {
		return false
	}
	for _, v := range uid {
		if !(v >= 'A' && v <= 'Z' || v >= 'a' && v <= 'z' || v >= '0' && v <= '9') {
			return false
		}
	}
	return true
}

func decodeReply(packet []byte, source netip.Addr, expectedUID string) (netip.Addr, error) {
	if !source.Is4() || !source.IsPrivate() || !source.IsGlobalUnicast() || len(packet) < 56 || len(packet) > 65507 || !bytes.Equal(packet[:16], []byte("ASZENO.SEARCH.V4")) {
		return netip.Addr{}, ErrDecode
	}
	seedLen := uint64(binary.LittleEndian.Uint32(packet[24:28]))
	cipherLen := uint64(binary.LittleEndian.Uint32(packet[28:32]))
	if seedLen > 32 || cipherLen < 520 || cipherLen%16 != 0 || 56+seedLen+cipherLen != uint64(len(packet)) {
		return netip.Addr{}, ErrDecode
	}
	start := 56 + int(seedLen)
	key := deriveKey(packet[56:start])
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return netip.Addr{}, ErrDecode
	}
	plaintext := make([]byte, int(cipherLen))
	cipher.NewCBCDecrypter(block, []byte("0000000000000000")).CryptBlocks(plaintext, packet[start:])
	uidField := plaintext[200:264]
	end := bytes.IndexByte(uidField, 0)
	if end < 0 || !validUID(uidField[:end]) {
		return netip.Addr{}, ErrDecode
	}
	if string(uidField[:end]) != expectedUID {
		return netip.Addr{}, errOtherUID
	}
	return source, nil
}

func deriveKey(seed []byte) (key [32]byte) {
	key[0] = firstKey
	for i := 1; i < len(key); i++ {
		key[i] = nextKey[key[i-1]]
	}
	for i, v := range seed {
		key[i] += v
	}
	var box [256]byte
	box[0] = firstBox[key[0]]
	for i := 1; i < len(box); i++ {
		box[i] = nextBox[box[i-1]]
	}
	// Native GenerateKey takes bytes 8:40 of this custom-S-box AES-256 schedule.
	var expanded [40]byte
	copy(expanded[:], key[:])
	for i := 32; i < 40; i += 4 {
		word := [4]byte{expanded[i-4], expanded[i-3], expanded[i-2], expanded[i-1]}
		if i == 32 {
			word = [4]byte{box[word[1]] ^ 1, box[word[2]], box[word[3]], box[word[0]]}
		}
		for j := 0; j < 4; j++ {
			expanded[i+j] = expanded[i+j-32] ^ word[j]
		}
	}
	copy(key[:], expanded[8:40])
	return key
}

type received struct {
	packet []byte
	source netip.Addr
	err    error
}
type udpTransport struct {
	sockets []*net.UDPConn
	packets chan received
	done    chan struct{}
}

func openUDP(ctx context.Context) (transport, error) {
	u := &udpTransport{packets: make(chan received, 4), done: make(chan struct{})}
	lc := net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error {
		var optionErr error
		err := raw.Control(func(fd uintptr) {
			optionErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
		})
		if err != nil {
			return err
		}
		return optionErr
	}}
	for _, address := range []string{"0.0.0.0:5001", "0.0.0.0:5003"} {
		conn, err := lc.ListenPacket(ctx, "udp4", address)
		if err != nil {
			u.close()
			if errors.Is(err, syscall.EADDRINUSE) {
				return nil, ErrPortInUse
			}
			return nil, ErrNetwork
		}
		u.sockets = append(u.sockets, conn.(*net.UDPConn))
	}
	for _, conn := range u.sockets {
		go u.read(conn)
	}
	return u, nil
}
func (u *udpTransport) read(conn *net.UDPConn) {
	for {
		packet := make([]byte, 65508)
		n, source, err := conn.ReadFromUDPAddrPort(packet)
		select {
		case u.packets <- received{packet[:n], source.Addr(), err}:
		case <-u.done:
			return
		}
		if err != nil {
			return
		}
	}
}
func (u *udpTransport) send() error {
	_, err := u.sockets[1].WriteToUDPAddrPort([]byte("ASZENO.SEARCH.V4.1"), netip.MustParseAddrPort("255.255.255.255:5000"))
	return err
}
func (u *udpTransport) receive(ctx context.Context) ([]byte, netip.Addr, error) {
	select {
	case p := <-u.packets:
		return p.packet, p.source, p.err
	case <-ctx.Done():
		return nil, netip.Addr{}, ctx.Err()
	}
}
func (u *udpTransport) close() {
	close(u.done)
	for _, conn := range u.sockets {
		conn.Close()
	}
}
