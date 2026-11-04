// Package runtime implements the private, managed helper's JSON-lines protocol.
package runtime

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"time"

	"github.com/FidelisClayton/ha-intelbras-allo/internal/bootstrap"
	"github.com/FidelisClayton/ha-intelbras-allo/internal/discovery"
	"github.com/FidelisClayton/ha-intelbras-allo/internal/media"
	"github.com/FidelisClayton/ha-intelbras-allo/internal/push"
	bridge "github.com/FidelisClayton/ha-intelbras-allo/internal/webrtc"
	pion "github.com/pion/webrtc/v4"
)

const maxLine = 2 << 20
const talkLease = 5 * time.Second

type request struct {
	ID             int64  `json:"id"`
	Op             string `json:"op"`
	Email          string `json:"email"`
	Password       string `json:"password"`
	DeviceID       string `json:"device_id"`
	Offer          string `json:"offer"`
	SessionID      string `json:"session_id"`
	StateDir       string `json:"state_dir"`
	Reauthenticate bool   `json:"reauthenticate"`
}
type safeError struct {
	Code string `json:"code"`
}
type response struct {
	ID     int64      `json:"id"`
	Result any        `json:"result,omitempty"`
	Error  *safeError `json:"error,omitempty"`
}
type runner struct {
	mu              sync.Mutex
	outputMu        sync.Mutex
	output          io.Writer
	client          *bootstrap.Client
	account         *bootstrap.Session
	creds           bootstrap.DeviceCredentials
	view            *view
	workers         sync.WaitGroup
	email, password string
	pushCancel      context.CancelFunc
	eventDeviceID   string
	retryDelay      func(context.Context, time.Duration) bool
	runPush         func(context.Context, push.Options, func(push.Event) error, func() error) error
}
type frameSource interface {
	ReadFrame(context.Context) (media.Frame, error)
	Close() error
}
type mediaPeer interface {
	WaitConnected(context.Context) error
	WriteVideo([]byte, time.Duration) error
	WriteAudio([]byte) error
	ReadMicrophone(context.Context) (bridge.MicrophonePacket, error)
	Close() error
}
type talkWriter interface {
	WritePCM([]int16) error
	Close() error
}
type view struct {
	id             string
	ctx            context.Context
	cancel         context.CancelFunc
	native         frameSource
	peer           mediaPeer
	once           sync.Once
	talkMu         sync.Mutex
	talk           talkWriter
	openTalk       func(context.Context) (talkWriter, error)
	talkTimer      *time.Timer
	talkGeneration uint64
	talkCancel     context.CancelFunc
}

// Run owns and closes input. EOF or cancellation cancels in-flight operations,
// tears down media, and waits for its readers before returning. Output is private
// authenticated IPC: the caller must not log it (device IDs and SDP are private).
func Run(parent context.Context, input io.ReadCloser, output io.Writer) error {
	return (&runner{runPush: push.Run}).run(parent, input, output)
}

func (r *runner) run(parent context.Context, input io.ReadCloser, output io.Writer) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var inputOnce sync.Once
	closeInput := func() { inputOnce.Do(func() { _ = input.Close() }) }
	stop := context.AfterFunc(ctx, closeInput)
	defer stop()
	defer closeInput()
	r.output = output
	defer func() {
		cancel()
		r.mu.Lock()
		v := r.view
		stopPush := r.pushCancel
		r.view = nil
		r.mu.Unlock()
		if stopPush != nil {
			stopPush()
		}
		if v != nil {
			v.close()
		}
		r.workers.Wait()
		if r.client != nil {
			r.client.Close()
		}
	}()
	lines := make(chan []byte)
	readDone := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 4096), maxLine+2)
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			if len(line) > maxLine {
				readDone <- errors.New("input_limit")
				cancel()
				close(lines)
				return
			}
			select {
			case lines <- line:
			case <-ctx.Done():
				readDone <- nil
				close(lines)
				return
			}
		}
		readDone <- scanner.Err()
		cancel()
		close(lines)
	}()
	var writeErr error
	for line := range lines {
		var req request
		err := json.Unmarshal(line, &req)
		resp := response{ID: req.ID}
		if err != nil || req.ID <= 0 || req.Op == "" {
			if err != nil || req.ID <= 0 {
				resp.ID = 0
			}
			resp.Error = &safeError{"invalid_request"}
		} else {
			result, code := r.dispatch(ctx, req)
			resp.Result = result
			if code != "" {
				resp.Error = &safeError{code}
			}
		}
		if err := r.write(resp); err != nil {
			writeErr = errors.New("output_failed")
			cancel()
		}
	}
	readErr := <-readDone
	if writeErr != nil {
		return writeErr
	}
	if readErr != nil && parent.Err() == nil {
		if err := r.write(response{ID: 0, Error: &safeError{"invalid_request"}}); err != nil {
			return errors.New("output_failed")
		}
		return errors.New("input_failed")
	}
	return nil
}
func (r *runner) write(value any) error {
	r.outputMu.Lock()
	defer r.outputMu.Unlock()
	return json.NewEncoder(r.output).Encode(value)
}
func (r *runner) dispatch(ctx context.Context, req request) (any, string) {
	r.mu.Lock()
	current := r.view
	eventsActive := r.pushCancel != nil
	r.mu.Unlock()
	if eventsActive && (req.Op == "login" || req.Op == "select_device" || req.Op == "start_events" || req.Op == "resume_events") {
		return nil, "busy"
	}
	switch req.Op {
	case "login", "select_device", "open_media", "prepare_media":
		if current != nil {
			return nil, "busy"
		}
	case "start_events", "resume_events":
	case "close_media", "talk_start", "talk_stop", "talk_keepalive":
		if current == nil || current.id != req.SessionID {
			return nil, "invalid_session"
		}
	default:
		return nil, "unknown_op"
	}
	switch req.Op {
	case "login":
		// A new explicit login invalidates the previous account and credentials.
		r.account = nil
		r.creds = bootstrap.DeviceCredentials{}
		r.email, r.password = "", ""
		if r.client != nil {
			r.client.Close()
		}
		if req.Email == "" || req.Password == "" {
			return nil, "invalid_request"
		}
		client, err := bootstrap.NewClient()
		if err != nil {
			return nil, "login_failed"
		}
		r.client = client
		account, err := client.Login(ctx, req.Email, req.Password)
		if err != nil {
			return nil, "login_failed"
		}
		devices, err := account.ListDevices(ctx)
		if err != nil {
			return nil, "device_list_failed"
		}
		r.account = account
		r.email, r.password = req.Email, req.Password
		type device struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			Model string `json:"model"`
		}
		list := make([]device, 0, len(devices))
		for _, d := range devices {
			list = append(list, device{d.ID, d.Name, d.Model})
		}
		return map[string]any{"devices": list}, ""
	case "select_device":
		if r.account == nil {
			return nil, "not_authenticated"
		}
		r.creds = bootstrap.DeviceCredentials{}
		if req.DeviceID == "" {
			return nil, "invalid_request"
		}
		creds, err := r.account.PrepareDeviceCredentials(ctx, req.DeviceID)
		if err != nil {
			return nil, "credentials_failed"
		}
		r.creds = creds
		return map[string]bool{"credentials_ready": true}, ""
	case "start_events":
		if r.creds.DeviceID() == "" {
			return nil, "credentials_required"
		}
		return r.startEventsWithOptions(ctx, push.Options{DeviceID: r.creds.DeviceID(), Email: r.email, Password: r.password, StateDir: req.StateDir, Reauthenticate: req.Reauthenticate})
	case "resume_events":
		if req.DeviceID == "" {
			return nil, "invalid_request"
		}
		return r.startEventsWithOptions(ctx, push.Options{DeviceID: req.DeviceID, StateDir: req.StateDir, ListenOnly: true})
	case "prepare_media":
		r.mu.Lock()
		selected := r.eventDeviceID
		r.mu.Unlock()
		if !eventsActive || selected == "" || req.DeviceID != selected {
			return nil, "invalid_request"
		}
		if req.Email == "" || req.Password == "" {
			return nil, "invalid_request"
		}
		client, err := bootstrap.NewClient()
		if err != nil {
			return nil, "login_failed"
		}
		account, err := client.Login(ctx, req.Email, req.Password)
		if err != nil {
			client.Close()
			return nil, "login_failed"
		}
		creds, err := account.PrepareDeviceCredentials(ctx, selected)
		if err != nil {
			client.Close()
			return nil, "credentials_failed"
		}
		if r.client != nil {
			r.client.Close()
		}
		r.client, r.account, r.creds = client, account, creds
		r.email, r.password = req.Email, req.Password
		return map[string]bool{"credentials_ready": true}, ""
	case "open_media":
		if r.creds.DeviceID() == "" {
			return nil, "credentials_required"
		}
		if req.Offer == "" {
			return nil, "invalid_request"
		}
		return r.open(ctx, req)
	case "close_media":
		r.finish(current, false)
		return map[string]bool{"closed": true}, ""
	case "talk_start":
		current.talkMu.Lock()
		defer current.talkMu.Unlock()
		if current.talk != nil {
			return nil, "busy"
		}
		talkCtx, cancel := context.WithCancel(current.ctx)
		talk, err := current.openTalk(talkCtx)
		if err != nil {
			cancel()
			return nil, "talk_failed"
		}
		current.talk, current.talkCancel = talk, cancel
		current.renewTalkLocked(talkLease)
		return map[string]bool{"talk_active": true}, ""
	case "talk_keepalive":
		current.talkMu.Lock()
		defer current.talkMu.Unlock()
		if current.talk == nil {
			return nil, "talk_inactive"
		}
		current.renewTalkLocked(talkLease)
		return map[string]bool{"talk_active": true}, ""
	case "talk_stop":
		current.stopTalk()
		return map[string]bool{"talk_active": false}, ""
	}
	return nil, "unknown_op"
}

// startEvents waits for bindings and the accepted MCS login, while the worker owns its lifetime.
func (r *runner) startEvents(ctx context.Context, deviceID, stateDir string) (any, string) {
	return r.startEventsWithOptions(ctx, push.Options{DeviceID: deviceID, Email: r.email, Password: r.password, StateDir: stateDir})
}
func (r *runner) startEventsWithOptions(ctx context.Context, options push.Options) (any, string) {
	if !filepath.IsAbs(options.StateDir) {
		return nil, "invalid_request"
	}
	pushCtx, cancel := context.WithCancel(ctx)
	r.mu.Lock()
	if r.pushCancel != nil {
		r.mu.Unlock()
		cancel()
		return nil, "busy"
	}
	r.pushCancel = cancel
	r.eventDeviceID = options.DeviceID
	r.mu.Unlock()
	ready := make(chan struct{})
	done := make(chan struct{})
	var once sync.Once
	var pushErr error
	availability := func(available bool) error {
		return r.write(struct {
			Event     string `json:"event"`
			Available bool   `json:"available"`
		}{"availability", available})
	}
	r.workers.Add(1)
	go func() {
		defer r.workers.Done()
		defer close(done)
		defer cancel()
		defer func() {
			_ = availability(false)
			r.mu.Lock()
			r.pushCancel = nil
			r.mu.Unlock()
		}()
		delay := time.Second
		everReady := false
		for {
			accepted := false
			pushErr = r.runPush(pushCtx, options, func(event push.Event) error {
				if !accepted || event.DeviceID != options.DeviceID || event.Channel != "1" {
					return nil
				}
				return r.write(struct {
					Event      string    `json:"event"`
					DeviceID   string    `json:"device_id"`
					Channel    int       `json:"channel"`
					ObservedAt time.Time `json:"observed_at"`
					SourceTime string    `json:"source_time,omitempty"`
				}{"doorbell", event.DeviceID, 1, event.ObservedAt.UTC(), event.SourceTime})
			}, func() error {
				if err := pushCtx.Err(); err != nil {
					return err
				}
				if err := availability(true); err != nil {
					return err
				}
				accepted = true
				everReady = true
				delay = time.Second
				once.Do(func() { close(ready) })
				return nil
			})
			if pushCtx.Err() != nil {
				return
			}
			var stage *push.Error
			if pushErr != nil && (!errors.As(pushErr, &stage) || stage.Stage != "listen") {
				if everReady {
					_ = r.write(struct {
						Event string `json:"event"`
						Code  string `json:"code"`
					}{"push_error", eventFailure(pushErr, options.ListenOnly)})
				}
				return
			}
			if err := availability(false); err != nil {
				return
			}
			options = push.Options{DeviceID: options.DeviceID, StateDir: options.StateDir, ListenOnly: true}
			if r.retryDelay != nil {
				if !r.retryDelay(pushCtx, delay) {
					return
				}
			} else {
				timer := time.NewTimer(delay)
				select {
				case <-timer.C:
				case <-pushCtx.Done():
					timer.Stop()
					return
				}
			}
			delay *= 2
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
		}
	}()
	timer := time.NewTimer(90 * time.Second)
	defer timer.Stop()
	select {
	case <-ready:
		return map[string]bool{"ready": true}, ""
	case <-done:
	case <-ctx.Done():
	case <-timer.C:
	}
	cancel()
	<-done
	return nil, eventFailure(pushErr, options.ListenOnly)
}
func eventFailure(err error, listenOnly bool) string {
	var stage *push.Error
	if errors.As(err, &stage) {
		if stage.Stage == "oauth_login" && stage.Code == "expired" {
			return "reauth_required"
		}
		if listenOnly && (stage.Stage == "state" || stage.Stage == "identity" || stage.Stage == "bindings" || stage.Stage == "oauth_login") {
			return "resume_required"
		}
	}
	return "events_failed"
}

func (r *runner) open(ctx context.Context, req request) (any, string) {
	discoveryCtx, stopDiscovery := context.WithTimeout(ctx, 5*time.Second)
	address, err := discovery.Discover(discoveryCtx, r.creds.DeviceID())
	stopDiscovery()
	if err != nil {
		return nil, "discovery_failed"
	}
	viewCtx, cancel := context.WithCancel(ctx)
	v := &view{ctx: viewCtx, cancel: cancel}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		cancel()
		return nil, "media_failed"
	}
	v.id = hex.EncodeToString(id[:])
	native, err := media.Open(viewCtx, media.Target{DeviceID: r.creds.DeviceID(), Host: address.String()}, r.creds, 1, 2)
	if err != nil {
		cancel()
		return nil, "media_failed"
	}
	v.native = native
	v.openTalk = func(ctx context.Context) (talkWriter, error) { return native.OpenTalk(ctx) }
	peer, err := bridge.New(viewCtx, pion.MimeTypePCMU)
	if err != nil {
		v.close()
		return nil, "media_failed"
	}
	v.peer = peer
	answer, err := peer.Answer(viewCtx, pion.SessionDescription{Type: pion.SDPTypeOffer, SDP: req.Offer})
	if err != nil {
		v.close()
		return nil, "invalid_offer"
	}
	r.mu.Lock()
	r.view = v
	r.mu.Unlock()
	r.workers.Add(2)
	go func() { defer r.workers.Done(); defer r.finish(v, true); v.pumpFrames() }()
	go func() { defer r.workers.Done(); defer r.finish(v, true); v.pumpMicrophone() }()
	return map[string]string{"session_id": v.id, "answer": answer.SDP}, ""
}
func (r *runner) finish(v *view, notify bool) {
	r.mu.Lock()
	owned := r.view == v
	if owned {
		r.view = nil
	}
	r.mu.Unlock()
	v.close()
	if owned && notify {
		_ = r.write(struct {
			Event     string `json:"event"`
			SessionID string `json:"session_id"`
		}{"media_closed", v.id})
	}
}
func (v *view) renewTalkLocked(duration time.Duration) {
	if v.talkTimer != nil {
		v.talkTimer.Stop()
	}
	v.talkGeneration++
	generation := v.talkGeneration
	v.talkTimer = time.AfterFunc(duration, func() { v.stopTalkGeneration(generation) })
}
func (v *view) stopTalk() { v.stopTalkGeneration(0) }
func (v *view) stopTalkGeneration(generation uint64) {
	v.talkMu.Lock()
	if generation != 0 && generation != v.talkGeneration {
		v.talkMu.Unlock()
		return
	}
	if v.talkTimer != nil {
		v.talkTimer.Stop()
		v.talkTimer = nil
	}
	v.talkGeneration++
	talk, cancel := v.talk, v.talkCancel
	v.talk, v.talkCancel = nil, nil
	if cancel != nil {
		cancel()
	}
	if talk != nil {
		_ = talk.Close()
	}
	v.talkMu.Unlock()
}
func (v *view) close() {
	v.once.Do(func() {
		v.cancel()
		v.stopTalk()
		if v.native != nil {
			_ = v.native.Close()
		}
		if v.peer != nil {
			_ = v.peer.Close()
		}
	})
}
func (v *view) pumpFrames() {
	if v.peer.WaitConnected(v.ctx) != nil {
		return
	}
	for {
		frame, err := v.native.ReadFrame(v.ctx)
		if err != nil {
			return
		}
		switch frame.Codec {
		case media.CodecH264:
			err = v.peer.WriteVideo(frame.Data, frame.Duration)
		case media.CodecPCM16:
			if len(frame.Data)%2 != 0 {
				return
			}
			pcm := make([]int16, len(frame.Data)/2)
			for i := range pcm {
				pcm[i] = int16(binary.LittleEndian.Uint16(frame.Data[2*i:]))
			}
			err = v.peer.WriteAudio(media.EncodeMuLaw(pcm))
		default:
			return
		}
		if err != nil {
			return
		}
	}
}
func (v *view) pumpMicrophone() {
	for {
		packet, err := v.peer.ReadMicrophone(v.ctx)
		if err != nil {
			return
		}
		if packet.Codec != pion.MimeTypePCMU || packet.Packet == nil {
			return
		}
		v.talkMu.Lock()
		talk := v.talk
		v.talkMu.Unlock()
		if talk == nil {
			continue
		}
		pcm := make([]int16, len(packet.Packet.Payload))
		for i, b := range packet.Packet.Payload {
			pcm[i] = media.DecodeMuLaw(b)
		}
		if err := talk.WritePCM(pcm); err != nil {
			if v.ctx.Err() != nil {
				return
			}
			v.talkMu.Lock()
			stillActive := v.talk == talk
			v.talkMu.Unlock()
			if stillActive {
				return
			}
		}
	}
}
