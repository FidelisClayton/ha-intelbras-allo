package receiver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang/protobuf/proto"
	fcm "github.com/morhaviv/go-fcm-receiver"
)

func config() Config {
	return Config{ProjectID: "synthetic-project", AppID: "test-app", APIKey: "synthetic-key", SenderID: "123", PackageName: "example.test", PackageCertSHA1: strings.Repeat("ab", 20), AppVersionCode: "19", AppVersionName: "2.4.77.17", DeviceID: "target"}
}
func state(t *testing.T) *State {
	t.Helper()
	s, err := NewState(config())
	if err != nil {
		t.Fatal(err)
	}
	s.AndroidID = 123
	s.SecurityToken = 456
	s.FCMToken = "synthetic"
	return s
}
func TestIdentity(t *testing.T) {
	a, _ := NewState(config())
	b, _ := NewState(config())
	if !ValidState(a) || a.FID == b.FID || a.ClientUUID == b.ClientUUID || len(a.FID) != 22 {
		t.Fatal("identity format or randomness")
	}
	bad := config()
	bad.PackageCertSHA1 = "invalid"
	if _, err := NewState(bad); err == nil {
		t.Fatal("accepted invalid certificate")
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(b []byte) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(b)), Header: http.Header{}}
}
func TestRegistrationPhasesAndResume(t *testing.T) {
	c := config()
	s, _ := NewState(c)
	calls := 0
	saves := 0
	client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		b, _ := io.ReadAll(r.Body)
		switch calls {
		case 1:
			if r.URL.Host != "firebaseinstallations.googleapis.com" || r.Header.Get("X-Android-Package") != c.PackageName || strings.Contains(string(b), "samsung") {
				t.Fatal("installation metadata")
			}
			var body map[string]string
			json.Unmarshal(b, &body)
			if body["fid"] != s.FID || body["sdkVersion"] != "a:17.0.0" {
				t.Fatal("installation body")
			}
			return response([]byte(`{"authToken":{"token":"installation"},"refreshToken":"refresh"}`)), nil
		case 2:
			var m fcm.AndroidCheckinRequest
			if proto.Unmarshal(b, &m) != nil || m.GetId() != 0 || m.GetSecurityToken() != 0 {
				t.Fatal("borrowed checkin identity")
			}
			data, _ := proto.Marshal(&fcm.AndroidCheckinResponse{StatsOk: proto.Bool(true), AndroidId: proto.Uint64(22), SecurityToken: proto.Uint64(33)})
			return response(data), nil
		case 3:
			v, _ := url.ParseQuery(string(b))
			if v.Get("app") != c.PackageName || v.Get("X-app_ver") != "19" || v.Get("X-app_ver_name") != c.AppVersionName || v.Get("X-Goog-Firebase-Installations-Auth") != "installation" || r.Header.Get("Authorization") != "AidLogin 22:33" || v.Get("X-Firebase-Client") != "" {
				t.Fatal("sender metadata")
			}
			return response([]byte("token=push-token")), nil
		default:
			t.Fatal("registration retried")
			return nil, errors.New("unexpected")
		}
	})}
	save := func(*State) error { saves++; return nil }
	if err := register(context.Background(), client, c, s, save); err != nil {
		t.Fatal(err)
	}
	if saves != 3 || s.FCMToken != "push-token" || s.InstallationRefreshToken != "refresh" {
		t.Fatal("state not persisted")
	}
	if err := register(context.Background(), client, c, s, save); err != nil || calls != 3 {
		t.Fatal("completed state registered again")
	}
}
func TestRegistrationFailurePreservesPhase(t *testing.T) {
	c := config()
	s, _ := NewState(c)
	calls := 0
	saves := 0
	client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return response([]byte(`{"authToken":{"token":"secret"},"refreshToken":"refresh"}`)), nil
		}
		return nil, errors.New("raw-secret-url")
	})}
	err := register(context.Background(), client, c, s, func(*State) error { saves++; return nil })
	if err == nil || strings.Contains(err.Error(), "secret") || calls != 2 || saves != 1 || s.InstallationAuthToken != "secret" {
		t.Fatal("failure lost phase or leaked")
	}
}
func TestHTTPBoundsAndAllowlist(t *testing.T) {
	called := false
	client := &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		called = true
		return response(bytes.Repeat([]byte("x"), (2<<20)+1)), nil
	})}
	for _, u := range []string{"http://android.clients.google.com/checkin", "https://evil.test/checkin", "https://android.clients.google.com:443/checkin", "https://android.clients.google.com.evil.test/checkin"} {
		if _, err := request(context.Background(), client, u, nil, nil); err == nil {
			t.Fatal("allowed endpoint")
		}
	}
	if called {
		t.Fatal("disallowed request reached transport")
	}
	if _, err := request(context.Background(), client, fcm.CheckInUrl, nil, nil); err == nil {
		t.Fatal("large response accepted")
	}
	gc := googleClient()
	if gc.Timeout != 15*time.Second || gc.CheckRedirect(nil, nil) == nil {
		t.Fatal("http safety defaults")
	}
}

type byteReader struct{ r io.Reader }

func (r byteReader) Read(b []byte) (int, error) { return r.r.Read(b[:1]) }
func TestFramesFragmentedAndMalformed(t *testing.T) {
	var b bytes.Buffer
	payload := &fcm.DataMessageStanza{From: proto.String("123"), Category: proto.String("example.test"), RawData: bytes.Repeat([]byte("x"), 400)}
	if err := writeFrame(&b, 8, payload); err != nil {
		t.Fatal(err)
	}
	tag, data, err := readFrame(bufio.NewReader(byteReader{&b}))
	var got fcm.DataMessageStanza
	if err != nil || tag != 8 || proto.Unmarshal(data, &got) != nil || len(got.RawData) != 400 {
		t.Fatal("fragmentation")
	}
	for _, data := range [][]byte{{8, 128, 128, 128, 128, 128}, {8, 255, 255, 255, 255, 16}, {8, 128, 128, 128, 1}, {8, 5, 1}} {
		if _, _, err := readFrame(bufio.NewReader(bytes.NewReader(data))); err == nil {
			t.Fatalf("accepted malformed %v", data)
		}
	}
}
func TestLoginCorrectFramingAndReplayIDs(t *testing.T) {
	s := state(t)
	s.PersistentIDs = []string{"a", "b", "c", "d"}
	var b bytes.Buffer
	if err := login(&b, s); err != nil {
		t.Fatal(err)
	}
	if b.Next(1)[0] != 41 {
		t.Fatal("version")
	}
	tag, data, err := readFrame(bufio.NewReader(&b))
	var m fcm.LoginRequest
	if err != nil || tag != 2 || proto.Unmarshal(data, &m) != nil || len(m.ReceivedPersistentId) != 4 || m.GetAuthToken() != "456" {
		t.Fatal("login framing or credentials")
	}
}
func runPipe(t *testing.T, s *State, save func(*State) error, emit func(Event) error, ready func() error) (net.Conn, *bufio.Reader, context.CancelFunc, <-chan error) {
	t.Helper()
	client, server := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- listenConn(ctx, client, config(), s, save, emit, ready, 50*time.Millisecond, 200*time.Millisecond)
	}()
	server.SetDeadline(time.Now().Add(2 * time.Second))
	r := bufio.NewReader(server)
	v, err := r.ReadByte()
	if err != nil || v != 41 {
		t.Fatal("client version")
	}
	if tag, _, err := readFrame(r); err != nil || tag != 2 {
		t.Fatal("client login")
	}
	t.Cleanup(func() { cancel(); server.Close() })
	return server, r, cancel, result
}
func serverLogin(t *testing.T, w io.Writer, errInfo *fcm.ErrorInfo) {
	t.Helper()
	w.Write([]byte{41})
	if err := writeFrame(w, 3, &fcm.LoginResponse{Id: proto.String("login"), Error: errInfo}); err != nil {
		t.Fatal(err)
	}
}
func TestRejectedLoginNeverReady(t *testing.T) {
	ready := false
	w, _, _, result := runPipe(t, state(t), func(*State) error { return nil }, func(Event) error { return nil }, func() error { ready = true; return nil })
	serverLogin(t, w, &fcm.ErrorInfo{Code: proto.Int32(401)})
	if err := <-result; err == nil || ready {
		t.Fatal("rejected login ready")
	}
}
func TestPingAckAndCancellation(t *testing.T) {
	ready := make(chan struct{})
	w, r, cancel, result := runPipe(t, state(t), func(*State) error { return nil }, func(Event) error { return nil }, func() error { close(ready); return nil })
	serverLogin(t, w, nil)
	<-ready
	if err := writeFrame(w, 0, &fcm.HeartbeatPing{}); err != nil {
		t.Fatal(err)
	}
	tag, b, err := readFrame(r)
	var ack fcm.HeartbeatAck
	if err != nil || tag != 1 || proto.Unmarshal(b, &ack) != nil || ack.GetLastStreamIdReceived() != 2 {
		t.Fatal("server ping not acknowledged")
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func dataMessage(id, device, kind string) *fcm.DataMessageStanza {
	m := &fcm.DataMessageStanza{From: proto.String("123"), Category: proto.String("example.test"), PersistentId: proto.String(id)}
	for k, v := range map[string]string{"DevUmid": device, "AlarmEvent": kind, "AlarmId": "alarm", "DevChNo": "0", "AlarmTime": "opaque time", "secret": "must not escape"} {
		m.AppData = append(m.AppData, &fcm.AppData{Key: proto.String(k), Value: proto.String(v)})
	}
	return m
}
func TestDataDedupFilteringAndPersistenceBeforeACK(t *testing.T) {
	s := state(t)
	saved := 0
	events := []Event{}
	w, r, cancel, result := runPipe(t, s, func(*State) error { saved++; return nil }, func(e Event) error { events = append(events, e); return nil }, func() error { return nil })
	serverLogin(t, w, nil)
	for _, m := range []*fcm.DataMessageStanza{dataMessage("one", "target", "19"), dataMessage("one", "target", "19"), dataMessage("two", "other", "19"), dataMessage("three", "target", "18")} {
		if err := writeFrame(w, 8, m); err != nil {
			t.Fatal(err)
		}
		tag, b, err := readFrame(r)
		var ack fcm.IqStanza
		if err != nil || tag != 7 || proto.Unmarshal(b, &ack) != nil || ack.Extension.GetId() != 13 || saved == 0 {
			t.Fatal("data ack")
		}
	}
	cancel()
	<-result
	if len(events) != 1 || events[0].Channel != "1" || len(s.PersistentIDs) != 3 || saved != 3 {
		t.Fatalf("filter/dedup: events %d saved %d", len(events), saved)
	}
	b, _ := json.Marshal(events)
	if strings.Contains(string(b), "secret") || strings.Contains(string(b), "alarm_id") {
		t.Fatal("raw payload leaked")
	}
}
func TestHeartbeatTimeout(t *testing.T) {
	w, r, _, result := runPipe(t, state(t), func(*State) error { return nil }, func(Event) error { return nil }, func() error { return nil })
	serverLogin(t, w, nil)
	if tag, _, err := readFrame(r); err != nil || tag != 0 {
		t.Fatal("no client heartbeat")
	}
	if err := <-result; err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatal(err)
	}
}
func TestSaveFailurePreventsACK(t *testing.T) {
	w, r, _, result := runPipe(t, state(t), func(*State) error { return errors.New("private detail") }, func(Event) error { return nil }, func() error { return nil })
	serverLogin(t, w, nil)
	writeFrame(w, 8, dataMessage("one", "target", "19"))
	if _, _, err := readFrame(r); err == nil {
		t.Fatal("ACK after save failure")
	}
	if err := <-result; err == nil || strings.Contains(err.Error(), "private detail") {
		t.Fatal("unsafe error")
	}
}

func TestStreamAckAndPersistentWindow(t *testing.T) {
	s := state(t)
	for i := 0; i < 1024; i++ {
		s.PersistentIDs = append(s.PersistentIDs, strings.Repeat("a", 2)+time.Unix(int64(i), 0).Format(time.RFC3339))
	}
	saved := false
	w, r, cancel, result := runPipe(t, s, func(*State) error { saved = true; return nil }, func(Event) error { return nil }, func() error { return nil })
	serverLogin(t, w, nil)
	writeFrame(w, 7, &fcm.IqStanza{Id: proto.String(""), Type: fcm.IqStanza_SET.Enum(), Extension: &fcm.Extension{Id: proto.Int32(13), Data: []byte{}}})
	tag, b, err := readFrame(r)
	var ack fcm.IqStanza
	if err != nil || tag != 7 || proto.Unmarshal(b, &ack) != nil || ack.GetLastStreamIdReceived() != 2 {
		t.Fatal("stream ACK")
	}
	writeFrame(w, 8, dataMessage("fresh", "target", "99"))
	if _, _, err := readFrame(r); err != nil {
		t.Fatal(err)
	}
	cancel()
	<-result
	if !saved || len(s.PersistentIDs) != 1024 || s.PersistentIDs[1023] != "fresh" {
		t.Fatal("unbounded persistent window")
	}
}

func TestEmitFailureDoesNotPersistOrACK(t *testing.T) {
	s := state(t)
	saved := false
	w, r, _, result := runPipe(t, s, func(*State) error { saved = true; return nil }, func(Event) error { return errors.New("private sink failure") }, func() error { return nil })
	serverLogin(t, w, nil)
	if err := writeFrame(w, 8, dataMessage("retry-me", "target", "19")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readFrame(r); err == nil {
		t.Fatal("ACK after emit failure")
	}
	if err := <-result; err == nil || strings.Contains(err.Error(), "private sink failure") {
		t.Fatal("unsafe or missing error")
	}
	if saved || len(s.PersistentIDs) != 0 {
		t.Fatal("failed emit was recorded as delivered")
	}
}

func TestPackageAndSenderFilters(t *testing.T) {
	var events int
	w, r, cancel, result := runPipe(t, state(t), func(*State) error { return nil }, func(Event) error { events++; return nil }, func() error { return nil })
	serverLogin(t, w, nil)
	for i := 0; i < 2; i++ {
		m := dataMessage(time.Unix(int64(i), 0).String(), "target", "19")
		if i == 0 {
			m.Category = proto.String("other.package")
		} else {
			m.From = proto.String("other-sender")
		}
		if writeFrame(w, 8, m) != nil {
			t.Fatal("send")
		}
		if _, _, err := readFrame(r); err != nil {
			t.Fatal(err)
		}
	}
	cancel()
	<-result
	if events != 0 {
		t.Fatal("untrusted sender emitted event")
	}
}
