package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/FidelisClayton/ha-intelbras-allo/internal/media"
	"github.com/FidelisClayton/ha-intelbras-allo/internal/push"
	bridge "github.com/FidelisClayton/ha-intelbras-allo/internal/webrtc"
	"github.com/pion/rtp"
	pion "github.com/pion/webrtc/v4"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestProtocolPrivacyAndValidation(t *testing.T) {
	input := `{"id":1,"op":"unknown-private-password"}
{"id":2,"op":"select_device","device_id":"private-device"}
{"id":3,"op":"close_media","session_id":"private-owner"}
{"id":4,"op":"login","email":"secret@example.invalid"}
{"id":5.5,"op":"unknown"}
{"id":6,"op":"unknown","password":"secret"} trailing
`
	var output bytes.Buffer
	if err := Run(context.Background(), io.NopCloser(strings.NewReader(input)), &output); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-password", "private-device", "private-owner", "secret@example.invalid", "trailing"} {
		if strings.Contains(output.String(), secret) {
			t.Fatalf("response leaked %s", secret)
		}
	}
	var got []string
	dec := json.NewDecoder(&output)
	for dec.More() {
		var response struct {
			ID    int `json:"id"`
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := dec.Decode(&response); err != nil {
			t.Fatal(err)
		}
		got = append(got, response.Error.Code)
	}
	want := "unknown_op,not_authenticated,invalid_session,invalid_request,invalid_request,invalid_request"
	if strings.Join(got, ",") != want {
		t.Fatalf("codes: %v", got)
	}
	// Each response is decoded above, so raw request text has no response field to occupy.
}

func TestMalformedAndOversizedInputNeverEchoed(t *testing.T) {
	for _, input := range []string{"private-password\n", strings.Repeat("s", maxLine+1) + "\n"} {
		var out bytes.Buffer
		_ = Run(context.Background(), io.NopCloser(strings.NewReader(input)), &out)
		if out.String() != "{\"id\":0,\"error\":{\"code\":\"invalid_request\"}}\n" {
			t.Fatalf("unsafe error response: %.100s", out.String())
		}
	}
}

func TestOpenMediaWithoutCredentialsIgnoresUnknownHost(t *testing.T) {
	input := `{"id":1,"op":"open_media","offer":"synthetic-offer"}
{"id":2,"op":"open_media","offer":"synthetic-offer","host":"private-unsupported-host"}
`
	var output bytes.Buffer
	if err := Run(context.Background(), io.NopCloser(strings.NewReader(input)), &output); err != nil {
		t.Fatal(err)
	}
	want := "{\"id\":1,\"error\":{\"code\":\"credentials_required\"}}\n" +
		"{\"id\":2,\"error\":{\"code\":\"credentials_required\"}}\n"
	if output.String() != want {
		t.Fatalf("responses: %s", output.String())
	}
}

func TestOpenDiscoveryFailureDoesNotCreateView(t *testing.T) {
	r := &runner{}
	// Empty synthetic credentials fail UID validation before discovery opens sockets.
	result, code := r.open(context.Background(), request{Offer: "synthetic-offer"})
	if result != nil || code != "discovery_failed" {
		t.Fatalf("open result = %v, code = %q", result, code)
	}
	if r.view != nil {
		t.Fatal("discovery failure retained a view")
	}
}

func TestCancelClosesBlockedInput(t *testing.T) {
	input, writer := io.Pipe()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, input, io.Discard) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestOwnerAndBusyState(t *testing.T) {
	r := &runner{view: &view{id: "owner"}}
	for _, op := range []string{"close_media", "talk_start", "talk_stop"} {
		_, code := r.dispatch(context.Background(), request{Op: op, SessionID: "other"})
		if code != "invalid_session" {
			t.Fatalf("%s: %s", op, code)
		}
	}
	for _, op := range []string{"login", "select_device", "open_media"} {
		_, code := r.dispatch(context.Background(), request{Op: op})
		if code != "busy" {
			t.Fatalf("%s: %s", op, code)
		}
	}
}

func TestEOFClosesOwnedInput(t *testing.T) {
	input := &trackedInput{Reader: strings.NewReader("")}
	if err := Run(context.Background(), input, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !input.closed {
		t.Fatal("input retained after EOF")
	}
}

type trackedInput struct {
	*strings.Reader
	closed bool
}

func (input *trackedInput) Close() error { input.closed = true; return nil }

func TestFailureReleasesPeerAndEmitsOneEvent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	peer, err := bridge.New(ctx, pion.MimeTypePCMU)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	v := &view{id: "synthetic-owner", ctx: ctx, cancel: cancel, peer: peer}
	var output bytes.Buffer
	r := &runner{view: v, output: &output}
	var calls sync.WaitGroup
	for range 2 {
		calls.Add(1)
		go func() { defer calls.Done(); r.finish(v, true) }()
	}
	calls.Wait()
	if r.view != nil {
		t.Fatal("failed view still owns session")
	}
	select {
	case <-peer.Done():
	default:
		t.Fatal("peer readers remain active")
	}
	if ctx.Err() == nil {
		t.Fatal("view context still active")
	}
	if output.String() != "{\"event\":\"media_closed\",\"session_id\":\"synthetic-owner\"}\n" {
		t.Fatalf("events: %s", output.String())
	}
}

func TestStartEventsRequiresCredentials(t *testing.T) {
	var out bytes.Buffer
	if err := Run(context.Background(), io.NopCloser(strings.NewReader("{\"id\":1,\"op\":\"start_events\",\"state_dir\":\"/private/state\"}\n")), &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "{\"id\":1,\"error\":{\"code\":\"credentials_required\"}}\n" {
		t.Fatalf("response: %s", out.String())
	}
}

func TestEventsReadyFilteringIdentityAndEOFJoin(t *testing.T) {
	var output bytes.Buffer
	entered, allowReady, returned, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	eventsSent := make(chan struct{})
	observed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("synthetic", 3600))
	r := &runner{output: &output, email: "synthetic@example.invalid", password: "private-password"}
	r.runPush = func(ctx context.Context, options push.Options, event func(push.Event) error, ready func() error) error {
		defer close(stopped)
		if options.DeviceID != "synthetic-device" || options.Email != r.email || options.Password != r.password || options.StateDir != "/synthetic/state" {
			t.Error("push options mismatch")
		}
		close(entered)
		// Even a package callback cannot generate a bell before readiness.
		if err := event(push.Event{DeviceID: options.DeviceID, Channel: "1", ObservedAt: observed}); err != nil {
			return err
		}
		select {
		case <-allowReady:
		case <-ctx.Done():
			return ctx.Err()
		}
		if err := ready(); err != nil {
			return err
		}
		for _, e := range []push.Event{
			{DeviceID: "foreign-device", Channel: "1", ObservedAt: observed},
			{DeviceID: options.DeviceID, Channel: "2", ObservedAt: observed},
			{DeviceID: options.DeviceID, Channel: "1", ObservedAt: observed, SourceTime: "synthetic-time", Source: "private-source", Kind: "private-kind"},
		} {
			if err := event(e); err != nil {
				return err
			}
		}
		close(eventsSent)
		<-ctx.Done()
		return ctx.Err()
	}
	go func() {
		defer close(returned)
		result, code := r.startEvents(context.Background(), "synthetic-device", "/synthetic/state")
		if code != "" || result.(map[string]bool)["ready"] != true {
			t.Errorf("start: %v %q", result, code)
		}
	}()
	<-entered
	select {
	case <-returned:
		t.Fatal("start_events returned before ready")
	default:
	}
	for _, op := range []string{"start_events", "login", "select_device"} {
		if _, code := r.dispatch(context.Background(), request{Op: op}); code != "busy" {
			t.Errorf("%s: %s", op, code)
		}
	}
	if _, code := r.dispatch(context.Background(), request{Op: "open_media"}); code != "credentials_required" {
		t.Fatalf("push incorrectly blocks media: %s", code)
	}
	close(allowReady)
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("start_events did not return after ready")
	}
	<-eventsSent
	// EOF owns cancellation and joins the worker before returning.
	if err := r.run(context.Background(), io.NopCloser(strings.NewReader("")), &output); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("EOF did not join push worker")
	}
	want := "{\"event\":\"availability\",\"available\":true}\n" +
		"{\"event\":\"doorbell\",\"device_id\":\"synthetic-device\",\"channel\":1,\"observed_at\":\"2026-01-02T02:04:05Z\",\"source_time\":\"synthetic-time\"}\n" +
		"{\"event\":\"availability\",\"available\":false}\n"
	if output.String() != want {
		t.Fatalf("events: %s", output.String())
	}
}

func TestEventsInvalidStateAndInitialFailure(t *testing.T) {
	var output bytes.Buffer
	r := &runner{output: &output, runPush: func(context.Context, push.Options, func(push.Event) error, func() error) error {
		return errors.New("private-remote-failure")
	}}
	for _, path := range []string{"", "relative/private"} {
		if _, code := r.startEvents(context.Background(), "synthetic-device", path); code != "invalid_request" {
			t.Errorf("path guard: %s", code)
		}
	}
	if output.Len() != 0 {
		t.Fatal("invalid request started listener")
	}
	if result, code := r.startEvents(context.Background(), "synthetic-device", "/synthetic/state"); result != nil || code != "events_failed" {
		t.Fatalf("failure: %v %s", result, code)
	}
	r.workers.Wait()
	if output.String() != "{\"event\":\"availability\",\"available\":false}\n" {
		t.Fatalf("unsafe failure: %s", output.String())
	}
	if r.pushCancel != nil {
		t.Fatal("failed worker retained ownership")
	}
}

func TestInvalidNewLoginClearsPrivateCredentials(t *testing.T) {
	r := &runner{email: "old@example.invalid", password: "private-password"}
	if _, code := r.dispatch(context.Background(), request{Op: "login"}); code != "invalid_request" {
		t.Fatal(code)
	}
	if r.email != "" || r.password != "" {
		t.Fatal("failed new login retained private credentials")
	}
}

func TestEventsCancellationBeforeReadinessJoinsWorker(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered, stopped := make(chan struct{}), make(chan struct{})
	var output bytes.Buffer
	r := &runner{output: &output, runPush: func(ctx context.Context, _ push.Options, _ func(push.Event) error, _ func() error) error {
		defer close(stopped)
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}}
	go func() { <-entered; cancel() }()
	if _, code := r.startEvents(ctx, "synthetic-device", "/synthetic/state"); code != "events_failed" {
		t.Fatal(code)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("startup cancellation did not join worker")
	}
	r.workers.Wait()
	if output.String() != "{\"event\":\"availability\",\"available\":false}\n" {
		t.Fatalf("events: %s", output.String())
	}
}

func TestKeepaliveRequiresActiveOwner(t *testing.T) {
	r := &runner{view: &view{id: "owner"}}
	if _, code := r.dispatch(context.Background(), request{Op: "talk_keepalive", SessionID: "other"}); code != "invalid_session" {
		t.Fatal(code)
	}
	if _, code := r.dispatch(context.Background(), request{Op: "talk_keepalive", SessionID: "owner"}); code != "talk_inactive" {
		t.Fatal(code)
	}
}
func TestEventsExpiredRequiresExplicitReauthentication(t *testing.T) {
	r := &runner{output: io.Discard, runPush: func(context.Context, push.Options, func(push.Event) error, func() error) error {
		return &push.Error{Stage: "oauth_login", Code: "expired"}
	}}
	if _, code := r.startEvents(context.Background(), "synthetic-device", "/synthetic/state"); code != "reauth_required" {
		t.Fatal(code)
	}
	r.workers.Wait()
}

func TestResumeReconnectUsesOnlyPersistedIdentityAndBoundedBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var delays []time.Duration
	calls := 0
	r := &runner{output: io.Discard, email: "must-not-reuse", password: "must-not-reuse"}
	r.retryDelay = func(ctx context.Context, d time.Duration) bool { delays = append(delays, d); return ctx.Err() == nil }
	r.runPush = func(ctx context.Context, o push.Options, _ func(push.Event) error, ready func() error) error {
		calls++
		if !o.ListenOnly || o.Reauthenticate || o.Email != "" || o.Password != "" || o.DeviceID != "synthetic-device" || o.StateDir != "/synthetic/state" {
			t.Error("reconnect attempted setup or changed identity")
		}
		if calls == 1 {
			if err := ready(); err != nil {
				return err
			}
			return nil
		}
		if calls < 9 {
			return &push.Error{Stage: "listen", Code: "failed"}
		}
		if err := ready(); err != nil {
			return err
		}
		if calls == 9 {
			return &push.Error{Stage: "listen", Code: "failed"}
		}
		cancel()
		return ctx.Err()
	}
	result, code := r.dispatch(ctx, request{Op: "resume_events", DeviceID: "synthetic-device", StateDir: "/synthetic/state"})
	// Readiness can race cancellation after the deterministic retry sequence.
	if code != "" && code != "events_failed" {
		t.Fatal(result, code)
	}
	r.workers.Wait()
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30, 30, 1}
	if len(delays) != len(want) {
		t.Fatal(delays)
	}
	for i, d := range delays {
		if d != want[i]*time.Second {
			t.Fatal(delays)
		}
	}
	if r.eventDeviceID != "synthetic-device" {
		t.Fatal("resume forgot selected receiver")
	}
}

func TestReconnectWaitCancellationAndNonListenFailure(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprint(failure), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			waiting := make(chan struct{})
			release := make(chan struct{})
			calls := 0
			r := &runner{output: io.Discard}
			r.retryDelay = func(ctx context.Context, _ time.Duration) bool { close(waiting); <-ctx.Done(); return false }
			r.runPush = func(ctx context.Context, o push.Options, _ func(push.Event) error, ready func() error) error {
				calls++
				if calls == 1 {
					if err := ready(); err != nil {
						return err
					}
					<-release
				}
				if failure {
					return &push.Error{Stage: "state", Code: "invalid"}
				}
				return &push.Error{Stage: "listen", Code: "failed"}
			}
			if _, code := r.startEvents(ctx, "synthetic-device", "/synthetic/state"); code != "" {
				t.Fatal(code)
			}
			close(release)
			if !failure {
				<-waiting
				cancel()
			}
			r.workers.Wait()
			if calls != 1 {
				t.Fatal("retried non-listen failure or canceled wait", calls)
			}
		})
	}
}

func TestPrepareMediaCannotChangeReceiver(t *testing.T) {
	r := &runner{eventDeviceID: "selected"}
	for _, id := range []string{"", "other"} {
		if _, code := r.dispatch(context.Background(), request{Op: "prepare_media", DeviceID: id, Email: "synthetic", Password: "synthetic"}); code != "invalid_request" {
			t.Fatal(code)
		}
	}
}

type fakeFrames struct{ frames chan media.Frame }

func (f *fakeFrames) ReadFrame(ctx context.Context) (media.Frame, error) {
	select {
	case v := <-f.frames:
		return v, nil
	case <-ctx.Done():
		return media.Frame{}, ctx.Err()
	}
}
func (f *fakeFrames) Close() error { return nil }

type videoSample struct {
	data     []byte
	duration time.Duration
}
type fakeMediaPeer struct {
	mic     chan bridge.MicrophonePacket
	audio   chan []byte
	video   chan videoSample
	reading chan struct{}
}

func (f *fakeMediaPeer) WaitConnected(context.Context) error { return nil }
func (f *fakeMediaPeer) WriteAudio(b []byte) error           { f.audio <- append([]byte(nil), b...); return nil }
func (f *fakeMediaPeer) WriteVideo(b []byte, d time.Duration) error {
	f.video <- videoSample{append([]byte(nil), b...), d}
	return nil
}
func (f *fakeMediaPeer) ReadMicrophone(ctx context.Context) (bridge.MicrophonePacket, error) {
	if f.reading != nil {
		select {
		case f.reading <- struct{}{}:
		case <-ctx.Done():
			return bridge.MicrophonePacket{}, ctx.Err()
		}
	}
	select {
	case p := <-f.mic:
		return p, nil
	case <-ctx.Done():
		return bridge.MicrophonePacket{}, ctx.Err()
	}
}
func (f *fakeMediaPeer) Close() error { return nil }

type fakeTalk struct {
	written chan []int16
	closed  chan struct{}
	once    sync.Once
	block   bool
}

func (f *fakeTalk) WritePCM(p []int16) error {
	f.written <- append([]int16(nil), p...)
	if f.block {
		<-f.closed
		return io.ErrClosedPipe
	}
	return nil
}
func (f *fakeTalk) Close() error { f.once.Do(func() { close(f.closed) }); return nil }

func TestMediaPumpsHoldOwnershipStopDuringWriteAndSecondHold(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source := &fakeFrames{make(chan media.Frame)}
	peer := &fakeMediaPeer{mic: make(chan bridge.MicrophonePacket), audio: make(chan []byte, 1), video: make(chan videoSample, 1), reading: make(chan struct{}, 1)}
	first := &fakeTalk{written: make(chan []int16, 1), closed: make(chan struct{}), block: true}
	second := &fakeTalk{written: make(chan []int16, 1), closed: make(chan struct{})}
	opened := 0
	v := &view{id: "owner", ctx: ctx, cancel: cancel, native: source, peer: peer, openTalk: func(context.Context) (talkWriter, error) {
		opened++
		if opened == 1 {
			return first, nil
		}
		return second, nil
	}}
	r := &runner{view: v, output: io.Discard}
	var workers sync.WaitGroup
	workers.Add(2)
	go func() { defer workers.Done(); v.pumpFrames() }()
	go func() { defer workers.Done(); v.pumpMicrophone() }()
	defer func() { v.close(); workers.Wait() }()
	// Native little-endian PCM is encoded into exact known PCMU values; H264 passes unchanged.
	source.frames <- media.Frame{Codec: media.CodecPCM16, Data: []byte{0, 0, 0xe8, 3, 0x18, 0xfc}}
	if got := <-peer.audio; !bytes.Equal(got, []byte{0xff, 0xce, 0x4e}) {
		t.Fatal(got)
	}
	h264 := []byte{0, 0, 0, 1, 0x65, 0x88}
	source.frames <- media.Frame{Codec: media.CodecH264, Data: h264, Duration: 40 * time.Millisecond}
	if got := <-peer.video; !bytes.Equal(got.data, h264) || got.duration != 40*time.Millisecond {
		t.Fatal(got)
	}
	packet := bridge.MicrophonePacket{Codec: pion.MimeTypePCMU, Packet: &rtp.Packet{Payload: []byte{0xff, 0xce, 0x4e}}}
	<-peer.reading
	peer.mic <- packet // Unheld microphone must be discarded before starting a hold.
	<-peer.reading
	if _, code := r.dispatch(ctx, request{Op: "talk_start", SessionID: "owner"}); code != "" {
		t.Fatal(code)
	}
	peer.mic <- packet
	if got := <-first.written; !reflect.DeepEqual(got, []int16{0, 988, -988}) {
		t.Fatal(got)
	}
	if _, code := r.dispatch(ctx, request{Op: "talk_stop", SessionID: "owner"}); code != "" {
		t.Fatal(code)
	}
	if ctx.Err() != nil || r.view != v {
		t.Fatal("stop closed receive view")
	}
	if _, code := r.dispatch(ctx, request{Op: "talk_start", SessionID: "owner"}); code != "" {
		t.Fatal(code)
	}
	peer.mic <- packet
	select {
	case <-second.written:
	case <-time.After(time.Second):
		t.Fatal("second hold lost microphone pump")
	}
	source.frames <- media.Frame{Codec: media.CodecH264, Data: h264}
	<-peer.video
}

func TestTalkLeaseExpiresWithoutStopAndRenewalPreservesHold(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := &fakeTalk{written: make(chan []int16, 1), closed: make(chan struct{})}
	second := &fakeTalk{written: make(chan []int16, 1), closed: make(chan struct{})}
	opened := 0
	v := &view{id: "owner", ctx: ctx, cancel: cancel, openTalk: func(context.Context) (talkWriter, error) {
		opened++
		if opened == 1 {
			return first, nil
		}
		return second, nil
	}}
	defer v.close()
	r := &runner{view: v}
	if _, code := r.dispatch(ctx, request{Op: "talk_start", SessionID: "owner"}); code != "" {
		t.Fatal(code)
	}
	v.talkMu.Lock()
	oldGeneration := v.talkGeneration
	v.renewTalkLocked(time.Millisecond)
	v.talkMu.Unlock()
	if _, code := r.dispatch(ctx, request{Op: "talk_keepalive", SessionID: "owner"}); code != "" {
		t.Fatal(code)
	}
	v.stopTalkGeneration(oldGeneration) // A stale expiry must not stop a renewed hold.
	select {
	case <-first.closed:
		t.Fatal("renewal closed talk")
	case <-time.After(15 * time.Millisecond):
	}
	v.talkMu.Lock()
	v.renewTalkLocked(time.Millisecond)
	v.talkMu.Unlock()
	select {
	case <-first.closed:
	case <-time.After(time.Second):
		t.Fatal("lease never closed TX")
	}
	if ctx.Err() != nil || r.view != v {
		t.Fatal("expiry closed receive view")
	}
	if _, code := r.dispatch(ctx, request{Op: "talk_start", SessionID: "owner"}); code != "" {
		t.Fatal(code)
	}
	v.stopTalkGeneration(oldGeneration)
	select {
	case <-second.closed:
		t.Fatal("old hold expiry stopped new hold")
	default:
	}
	if talkLease != 5*time.Second {
		t.Fatal("lease must be five seconds")
	}
}

func TestResumeStateFailureIsFixedAndNotRetried(t *testing.T) {
	for _, stage := range []string{"state", "bindings", "oauth_login"} {
		t.Run(stage, func(t *testing.T) {
			calls := 0
			r := &runner{output: io.Discard, runPush: func(context.Context, push.Options, func(push.Event) error, func() error) error {
				calls++
				return &push.Error{Stage: stage, Code: "invalid"}
			}}
			if _, code := r.dispatch(context.Background(), request{Op: "resume_events", DeviceID: "synthetic", StateDir: "/synthetic"}); code != "resume_required" {
				t.Fatal(code)
			}
			r.workers.Wait()
			if calls != 1 {
				t.Fatal(calls)
			}
		})
	}
}

func TestTerminalFailureAfterReadyEmitsFixedPushError(t *testing.T) {
	var output bytes.Buffer
	calls := 0
	release := make(chan struct{})
	r := &runner{output: &output, retryDelay: func(context.Context, time.Duration) bool { return true }}
	r.runPush = func(_ context.Context, _ push.Options, _ func(push.Event) error, ready func() error) error {
		calls++
		if calls == 1 {
			if err := ready(); err != nil {
				return err
			}
			<-release
			return &push.Error{Stage: "listen", Code: "failed"}
		}
		return &push.Error{Stage: "state", Code: "identity_mismatch"}
	}
	_, code := r.startEvents(context.Background(), "synthetic-device", "/synthetic/state")
	if code != "" {
		t.Fatal(code)
	}
	close(release)
	r.workers.Wait()
	want := "{\"event\":\"availability\",\"available\":true}\n{\"event\":\"availability\",\"available\":false}\n{\"event\":\"push_error\",\"code\":\"resume_required\"}\n{\"event\":\"availability\",\"available\":false}\n"
	if output.String() != want {
		t.Fatal(output.String())
	}
}

func TestTalkStartLeaseExpiresWithoutAnyStopRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	talk := &fakeTalk{written: make(chan []int16, 1), closed: make(chan struct{})}
	v := &view{id: "owner", ctx: ctx, cancel: cancel, openTalk: func(context.Context) (talkWriter, error) { return talk, nil }}
	defer v.close()
	r := &runner{view: v}
	if _, code := r.dispatch(ctx, request{Op: "talk_start", SessionID: "owner"}); code != "" {
		t.Fatal(code)
	}
	select {
	case <-talk.closed:
	case <-time.After(talkLease + time.Second):
		t.Fatal("talk_start lacked hard lease")
	}
	if ctx.Err() != nil || r.view != v {
		t.Fatal("lease closed receive view")
	}
	if _, code := r.dispatch(ctx, request{Op: "talk_keepalive", SessionID: "owner"}); code != "talk_inactive" {
		t.Fatal(code)
	}
}

func TestSetupRetryDropsCredentialsAndReauthenticate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	calls := 0
	r := &runner{output: io.Discard, retryDelay: func(context.Context, time.Duration) bool { return true }}
	r.runPush = func(ctx context.Context, o push.Options, _ func(push.Event) error, ready func() error) error {
		calls++
		if calls == 1 {
			if o.ListenOnly || !o.Reauthenticate || o.Email != "synthetic@example.invalid" || o.Password != "synthetic-password" {
				t.Error("initial setup options lost")
			}
			if err := ready(); err != nil {
				return err
			}
			<-release
			return &push.Error{Stage: "listen", Code: "failed"}
		}
		if !o.ListenOnly || o.Reauthenticate || o.Email != "" || o.Password != "" || o.DeviceID != "synthetic-device" || o.StateDir != "/synthetic/state" {
			t.Error("retry retained setup authority")
		}
		cancel()
		return ctx.Err()
	}
	if _, code := r.startEventsWithOptions(ctx, push.Options{DeviceID: "synthetic-device", StateDir: "/synthetic/state", Email: "synthetic@example.invalid", Password: "synthetic-password", Reauthenticate: true}); code != "" {
		t.Fatal(code)
	}
	close(release)
	r.workers.Wait()
	if calls != 2 {
		t.Fatal(calls)
	}
}
