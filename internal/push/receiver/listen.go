package receiver

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/golang/protobuf/proto"
	fcm "github.com/morhaviv/go-fcm-receiver"
)

type Event struct {
	Schema     int       `json:"schema"`
	Source     string    `json:"source"`
	Kind       string    `json:"kind"`
	DeviceID   string    `json:"device_id"`
	Channel    string    `json:"channel"`
	SourceTime string    `json:"source_time"`
	ObservedAt time.Time `json:"observed_at"`
}

// ParseEvent exposes only the APK's known alarm fields; unsupported payloads are ignored.
func ParseEvent(c Config, data map[string]string) (Event, bool) {
	if data["AlarmEvent"] != "19" || data["DevUmid"] != c.DeviceID || data["CameraId"] != "" || data["SrcAccountId"] != "" || data["message_content"] != "" {
		return Event{}, false
	}
	channel := "1"
	if v, ok := data["DevChNo"]; ok {
		n, err := strconv.ParseUint(v, 10, 31)
		if err != nil {
			return Event{}, false
		}
		channel = strconv.FormatUint(n+1, 10)
	}
	for _, k := range []string{"AlarmId", "DevUmid", "AlarmTime"} {
		if len(data[k]) > 1024 {
			return Event{}, false
		}
	}
	return Event{Schema: 1, Source: "google_push", Kind: "doorbell", DeviceID: data["DevUmid"], Channel: channel, SourceTime: data["AlarmTime"], ObservedAt: time.Now().UTC()}, true
}

type frame struct {
	tag  byte
	data []byte
	err  error
}

func readFrame(r *bufio.Reader) (byte, []byte, error) {
	tag, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	var n uint32
	for i := 0; i < 5; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return 0, nil, err
		}
		if i == 4 && b > 15 {
			return 0, nil, errors.New("invalid frame length")
		}
		n |= uint32(b&127) << uint(7*i)
		if b&128 == 0 {
			if n > 1<<20 {
				return 0, nil, errors.New("frame too large")
			}
			data := make([]byte, n)
			_, err := io.ReadFull(r, data)
			return tag, data, err
		}
	}
	return 0, nil, errors.New("invalid frame length")
}
func writeFrame(w io.Writer, tag byte, m proto.Message) error {
	b, err := proto.Marshal(m)
	if err != nil || len(b) > 1<<20 {
		return errors.New("push frame encoding failed")
	}
	var size [5]byte
	n := binary.PutUvarint(size[:], uint64(len(b)))
	packet := append([]byte{tag}, size[:n]...)
	packet = append(packet, b...)
	for len(packet) > 0 {
		n, err := w.Write(packet)
		if err != nil {
			return errors.New("push write failed")
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		packet = packet[n:]
	}
	return nil
}
func login(w io.Writer, s *State) error {
	// Upstream helper emits an incorrect fixed length and truncates replay IDs.
	raw, err := fcm.CreateLoginRequestRaw(&s.AndroidID, &s.SecurityToken, nil)
	if err != nil || len(raw) < 4 {
		return errors.New("push login encoding failed")
	}
	var m fcm.LoginRequest
	if proto.Unmarshal(raw[4:], &m) != nil {
		return errors.New("push login encoding failed")
	}
	m.ReceivedPersistentId = s.PersistentIDs
	if _, err := w.Write([]byte{fcm.KMCSVersion}); err != nil {
		return errors.New("push write failed")
	}
	return writeFrame(w, fcm.KLoginRequestTag, &m)
}

func Listen(ctx context.Context, c Config, s *State, save func(*State) error, emit func(Event) error, ready func() error) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if !ValidState(s) || s.AndroidID == 0 || s.FCMToken == "" || save == nil || emit == nil || ready == nil {
		return errors.New("push state or callbacks invalid")
	}
	dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: 15 * time.Second}, Config: &tls.Config{ServerName: "mtalk.google.com", MinVersion: tls.VersionTLS12}}
	conn, err := dialer.DialContext(ctx, "tcp", fcm.FcmSocketAddress)
	if err != nil {
		return errors.New("Google push connection failed")
	}
	return listenConn(ctx, conn, c, s, save, emit, ready, 30*time.Second, 15*time.Second)
}
func listenConn(ctx context.Context, conn net.Conn, c Config, s *State, save func(*State) error, emit func(Event) error, ready func() error, interval, timeout time.Duration) error {
	defer conn.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()
	conn.SetWriteDeadline(time.Now().Add(timeout))
	if err := login(conn, s); err != nil {
		return err
	}
	frames := make(chan frame, 1)
	go func() {
		r := bufio.NewReader(conn)
		v, err := r.ReadByte()
		if err == nil && v != fcm.KMCSVersion {
			err = errors.New("push protocol version rejected")
		}
		if err != nil {
			select {
			case frames <- frame{err: err}:
			case <-done:
			}
			return
		}
		for {
			tag, b, err := readFrame(r)
			select {
			case frames <- frame{tag: tag, data: b, err: err}:
			case <-done:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	heartbeat := time.NewTicker(interval)
	defer heartbeat.Stop()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	loggedIn := false
	waiting := false
	stream := int32(0)
	send := func(tag byte, m proto.Message) error {
		conn.SetWriteDeadline(time.Now().Add(timeout))
		return writeFrame(conn, tag, m)
	}
	ack := func() error {
		return send(fcm.KIqStanzaTag, &fcm.IqStanza{Type: fcm.IqStanza_SET.Enum(), Id: proto.String(""), LastStreamIdReceived: proto.Int32(stream), Extension: &fcm.Extension{Id: proto.Int32(13), Data: []byte{}}})
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("push login or heartbeat timeout")
		case <-heartbeat.C:
			if loggedIn && !waiting {
				if err := send(fcm.KHeartbeatPingTag, &fcm.HeartbeatPing{LastStreamIdReceived: proto.Int32(stream)}); err != nil {
					return err
				}
				waiting = true
				deadline.Reset(timeout)
			}
		case f := <-frames:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if f.err != nil {
				return errors.New("push stream ended or frame invalid")
			}
			stream++
			if !loggedIn && f.tag != fcm.KLoginResponseTag {
				return errors.New("push login response required")
			}
			switch f.tag {
			case fcm.KLoginResponseTag:
				var m fcm.LoginResponse
				if loggedIn || proto.Unmarshal(f.data, &m) != nil || m.Error != nil {
					return errors.New("push login rejected")
				}
				loggedIn = true
				deadline.Stop()
				if err := ready(); err != nil {
					return errors.New("push readiness callback failed")
				}
			case fcm.KHeartbeatPingTag:
				var m fcm.HeartbeatPing
				if proto.Unmarshal(f.data, &m) != nil {
					return errors.New("push heartbeat invalid")
				}
				if err := send(fcm.KHeartbeatAckTag, &fcm.HeartbeatAck{LastStreamIdReceived: proto.Int32(stream)}); err != nil {
					return err
				}
			case fcm.KHeartbeatAckTag:
				var m fcm.HeartbeatAck
				if proto.Unmarshal(f.data, &m) != nil {
					return errors.New("push heartbeat invalid")
				}
				if waiting {
					waiting = false
					deadline.Stop()
				}
			case fcm.KCloseTag, fcm.KStreamErrorStanzaTag:
				return errors.New("Google push stream closed")
			case fcm.KIqStanzaTag:
				var m fcm.IqStanza
				if proto.Unmarshal(f.data, &m) != nil || m.Error != nil {
					return errors.New("push IQ invalid")
				}
				if x := m.Extension; x != nil {
					switch x.GetId() {
					case 12:
						var a fcm.SelectiveAck
						if proto.Unmarshal(x.Data, &a) != nil {
							return errors.New("push selective ack invalid")
						}
					case 13:
						var a fcm.StreamAck
						if proto.Unmarshal(x.Data, &a) != nil {
							return errors.New("push stream ack invalid")
						}
						if m.GetType() == fcm.IqStanza_GET || m.GetType() == fcm.IqStanza_SET {
							if err := ack(); err != nil {
								return err
							}
						}
					default:
						return errors.New("push IQ extension unsupported")
					}
				}
			case fcm.KDataMessageStanzaTag:
				var m fcm.DataMessageStanza
				if proto.Unmarshal(f.data, &m) != nil {
					return errors.New("push data invalid")
				}
				id := m.GetPersistentId()
				duplicate := false
				for _, seen := range s.PersistentIDs {
					if id != "" && seen == id {
						duplicate = true
						break
					}
				}
				if !duplicate {
					data := map[string]string{}
					for _, a := range m.AppData {
						data[a.GetKey()] = a.GetValue()
					}
					if m.GetCategory() == c.PackageName && m.GetFrom() == c.SenderID {
						if event, ok := ParseEvent(c, data); ok {
							if err := emit(event); err != nil {
								return errors.New("push event callback failed")
							}
						}
					}
					// Emit, persist, then ACK; a crash between phases can duplicate delivery.
					if id != "" {
						if len(id) > 4096 {
							return errors.New("push persistent ID invalid")
						}
						s.PersistentIDs = append(s.PersistentIDs, id)
						if len(s.PersistentIDs) > 1024 {
							s.PersistentIDs = s.PersistentIDs[len(s.PersistentIDs)-1024:]
						}
						if err := save(s); err != nil {
							return errors.New("push state save failed")
						}
					}
				}
				if err := ack(); err != nil {
					return err
				}
			default:
				return errors.New("push frame unsupported")
			}
		}
	}
}
