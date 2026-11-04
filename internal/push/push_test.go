package push

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FidelisClayton/ha-intelbras-allo/internal/push/intelbras"
	"github.com/FidelisClayton/ha-intelbras-allo/internal/push/receiver"
)

type fakeAccount struct {
	login func(context.Context, intelbras.Config, intelbras.Credentials, string, *intelbras.AuthState, func(*intelbras.AuthState) error) error
	bind  func(context.Context, intelbras.Config, string, string, *intelbras.AuthState, func(*intelbras.AuthState) error) error
}

func (f fakeAccount) Login(c context.Context, g intelbras.Config, p intelbras.Credentials, id string, s *intelbras.AuthState, save func(*intelbras.AuthState) error) error {
	return f.login(c, g, p, id, s, save)
}
func (f fakeAccount) Bind(c context.Context, g intelbras.Config, id, token string, s *intelbras.AuthState, save func(*intelbras.AuthState) error) error {
	return f.bind(c, g, id, token, s, save)
}

type fakeAlarm func(context.Context, intelbras.Config, intelbras.Credentials, string, string, *intelbras.AuthState, func(*intelbras.AuthState) error) error

func (f fakeAlarm) Register(c context.Context, g intelbras.Config, p intelbras.Credentials, id, token string, s *intelbras.AuthState, save func(*intelbras.AuthState) error) error {
	return f(c, g, p, id, token, s, save)
}
func jwt(id string) string {
	b, _ := json.Marshal(map[string]any{"cli-id": "003-4077-" + id, "qv-uid": "123456"})
	return "header." + base64.RawURLEncoding.EncodeToString(b) + ".signature"
}
func options(t *testing.T) Options {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	return Options{DeviceID: "synthetic-selected", Email: "a@example.invalid", Password: "raw-private-password", StateDir: dir}
}
func callbacks() (func(Event) error, func() error) {
	return func(Event) error { return nil }, func() error { return nil }
}

func TestNewInstallationProgressIdentityAndResume(t *testing.T) {
	o := options(t)
	var identity string
	var sequence []string
	failNative := true
	b := backend{}
	b.register = func(_ context.Context, _ receiver.Config, s *receiver.State, save func(*receiver.State) error) error {
		var disk savedState
		if readPrivate(filepath.Join(o.StateDir, "push.json"), 1<<20, &disk) != nil || disk.Google.ClientUUID != s.ClientUUID || disk.Google.FID != s.FID {
			t.Fatal("identity not saved before first request")
		}
		identity = s.ClientUUID
		sequence = append(sequence, "google")
		s.AndroidID = 1
		s.SecurityToken = 2
		s.FCMToken = "own-token"
		return save(s)
	}
	b.account = fakeAccount{
		login: func(_ context.Context, c intelbras.Config, p intelbras.Credentials, id string, s *intelbras.AuthState, save func(*intelbras.AuthState) error) error {
			if id != identity || c.RegionID != "1" || p.Password != o.Password {
				t.Fatal("wrong OAuth identity/input")
			}
			sequence = append(sequence, "oauth")
			*s = intelbras.AuthState{ClientUUID: id, AccessToken: jwt(id), RefreshToken: "own-refresh", ExpiresAt: time.Now().Add(time.Hour)}
			return save(s)
		},
		bind: func(_ context.Context, c intelbras.Config, id, token string, s *intelbras.AuthState, save func(*intelbras.AuthState) error) error {
			if id != identity || c.UserID != "123456" || c.SystemVersion != "11" || c.Language != "pt" || token != "own-token" {
				t.Fatal("wrong binding identity/input")
			}
			sequence = append(sequence, "modern")
			s.InfoRegistered = true
			s.TokenRegistered = true
			return save(s)
		},
	}
	b.alarm = fakeAlarm(func(_ context.Context, c intelbras.Config, p intelbras.Credentials, id, token string, s *intelbras.AuthState, save func(*intelbras.AuthState) error) error {
		if c.UserID != "123456" || id != identity || token != "own-token" || !s.TokenRegistered || p.Password != o.Password {
			t.Fatal("native stage before verified modern identity")
		}
		sequence = append(sequence, "native")
		if failNative {
			return errors.New("private remote URL/password")
		}
		s.AlarmRegistered = true
		s.AlarmSessionID = "own-session"
		return save(s)
	})
	ready := 0
	b.listen = func(_ context.Context, _ receiver.Config, s *receiver.State, _ func(*receiver.State) error, _ func(Event) error, onReady func() error) error {
		sequence = append(sequence, "listen")
		if s.ClientUUID != identity {
			t.Fatal("identity changed")
		}
		return onReady()
	}
	emit, _ := callbacks()
	err := run(context.Background(), o, emit, func() error { ready++; return nil }, b)
	if err == nil || err.Error() != "native_binding:failed" || ready != 0 {
		t.Fatal("registration failure reported ready or leaked", err)
	}
	failNative = false
	if err := run(context.Background(), o, emit, func() error { ready++; return nil }, b); err != nil {
		t.Fatal(err)
	}
	if strings.Join(sequence, ",") != "google,oauth,modern,native,native,listen" || ready != 1 {
		t.Fatal(sequence, ready)
	}
	data, err := os.ReadFile(filepath.Join(o.StateDir, "push.json"))
	if err != nil || strings.Contains(string(data), o.Password) || strings.Contains(string(data), o.Email) {
		t.Fatal("credentials persisted")
	}
	// A later listener restart needs no account credentials and does not bind again.
	o.Email = ""
	o.Password = ""
	if err := run(context.Background(), o, emit, func() error { ready++; return nil }, b); err != nil {
		t.Fatal(err)
	}
	if strings.Join(sequence, ",") != "google,oauth,modern,native,native,listen,listen" {
		t.Fatal(sequence)
	}
}

func TestPendingExpiredOrMismatchedOAuthNeverRefreshesOrListens(t *testing.T) {
	for _, kind := range []string{"expired", "other-client", "bad-account", "selected-device"} {
		t.Run(kind, func(t *testing.T) {
			o := options(t)
			var cfg receiver.Config
			json.Unmarshal(appJSON, &cfg)
			cfg.DeviceID = o.DeviceID
			google, _ := receiver.NewState(cfg)
			google.AndroidID = 1
			google.SecurityToken = 2
			google.FCMToken = "own-token"
			auth := intelbras.AuthState{ClientUUID: google.ClientUUID, AccessToken: jwt(google.ClientUUID), ExpiresAt: time.Now().Add(time.Hour), InfoRegistered: true, TokenRegistered: true, AlarmRegistered: true}
			if kind == "expired" {
				auth.ExpiresAt = time.Now().Add(-time.Hour)
				auth.AlarmRegistered = false
			}
			if kind == "other-client" {
				auth.AccessToken = jwt("bbbbbbbb-bbbb-4ccc-8ddd-eeeeeeeeeeee")
			}
			if kind == "bad-account" {
				auth.AccessToken = "invalid"
			}
			s := savedState{1, o.DeviceID, google, auth}
			if savePrivate(filepath.Join(o.StateDir, "push.json"), &s) != nil {
				t.Fatal("save")
			}
			if kind == "selected-device" {
				o.DeviceID = "another"
			}
			emit, ready := callbacks()
			err := run(context.Background(), o, emit, ready, backend{})
			if err == nil {
				t.Fatal("invalid saved identity accepted")
			}
			if kind == "expired" && err.Error() != "oauth_login:expired" {
				t.Fatal("unexpected expired-stage error", err)
			}
		})
	}
}
func TestCancelAndPrivateStateValidationBeforeNetwork(t *testing.T) {
	o := options(t)
	emit, ready := callbacks()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run(ctx, o, emit, ready, backend{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Stat(o.StateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancel created state")
	}
	os.Mkdir(o.StateDir, 0755)
	if err := run(context.Background(), o, emit, ready, backend{}); err == nil {
		t.Fatal("unsafe state dir accepted")
	}
}
func TestStateLockAndTimezones(t *testing.T) {
	o := options(t)
	os.Mkdir(o.StateDir, 0700)
	path := filepath.Join(o.StateDir, "push.lock")
	lock, err := lockState(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if other, err := lockState(path); err == nil {
		other.Close()
		t.Fatal("concurrent receiver accepted")
	}
	for _, offset := range []int{-10800, 0, 19800} {
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.FixedZone("synthetic", offset))
		want := map[int]string{-10800: "gmt-03:00", 0: "gmt+00:00", 19800: "gmt+05:30"}[offset]
		if timezone(now) != want {
			t.Fatal(timezone(now))
		}
	}
}

func TestMissingCredentialsNeverRegistersGoogle(t *testing.T) {
	o := options(t)
	o.Password = ""
	emit, ready := callbacks()
	b := backend{register: func(context.Context, receiver.Config, *receiver.State, func(*receiver.State) error) error {
		t.Fatal("registration before required credentials")
		return nil
	}}
	err := run(context.Background(), o, emit, ready, b)
	if err == nil || err.Error() != "oauth_login:credentials_required" {
		t.Fatal(err)
	}
}

func TestCompletedBindingsListenWithExpiredOAuthAndNoPassword(t *testing.T) {
	o := options(t)
	var cfg receiver.Config
	json.Unmarshal(appJSON, &cfg)
	cfg.DeviceID = o.DeviceID
	google, _ := receiver.NewState(cfg)
	google.AndroidID = 1
	google.SecurityToken = 2
	google.FCMToken = "own-token"
	auth := intelbras.AuthState{ClientUUID: google.ClientUUID, AccessToken: jwt(google.ClientUUID), ExpiresAt: time.Now().Add(-time.Hour), InfoRegistered: true, TokenRegistered: true, AlarmRegistered: true}
	state := savedState{1, o.DeviceID, google, auth}
	if err := savePrivate(filepath.Join(o.StateDir, "push.json"), &state); err != nil {
		t.Fatal(err)
	}
	o.Email = ""
	o.Password = ""
	listenCalls, readyCalls := 0, 0
	b := backend{listen: func(_ context.Context, _ receiver.Config, s *receiver.State, _ func(*receiver.State) error, _ func(Event) error, ready func() error) error {
		listenCalls++
		if s.ClientUUID != google.ClientUUID || s.AndroidID != 1 || s.SecurityToken != 2 {
			t.Fatal("saved Google identity changed")
		}
		return ready()
	}}
	emit, _ := callbacks()
	if err := run(context.Background(), o, emit, func() error { readyCalls++; return nil }, b); err != nil {
		t.Fatal(err)
	}
	if listenCalls != 1 || readyCalls != 1 {
		t.Fatal("completed bindings never reached listener readiness")
	}
}

func TestExplicitReauthenticationPreservesIdentityAndBindings(t *testing.T) {
	for _, outcome := range []string{"success", "account-mismatch", "login-failure"} {
		t.Run(outcome, func(t *testing.T) {
			o := options(t)
			var cfg receiver.Config
			json.Unmarshal(appJSON, &cfg)
			cfg.DeviceID = o.DeviceID
			google, _ := receiver.NewState(cfg)
			google.AndroidID, google.SecurityToken, google.FCMToken = 1, 2, "own-token"
			old := intelbras.AuthState{ClientUUID: google.ClientUUID, AccessToken: jwt(google.ClientUUID), ExpiresAt: time.Now().Add(-time.Hour), InfoRegistered: true, TokenRegistered: true, AlarmSessionID: "retained-session"}
			s := savedState{1, o.DeviceID, google, old}
			path := filepath.Join(o.StateDir, "push.json")
			if err := savePrivate(path, &s); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(path)
			emit, ready := callbacks()
			if err := run(context.Background(), o, emit, ready, backend{}); err == nil || err.Error() != "oauth_login:expired" {
				t.Fatal(err)
			}
			logins, bindings := 0, 0
			b := backend{account: fakeAccount{login: func(_ context.Context, _ intelbras.Config, _ intelbras.Credentials, id string, a *intelbras.AuthState, save func(*intelbras.AuthState) error) error {
				logins++
				if id != google.ClientUUID {
					t.Fatal("changed client identity")
				}
				*a = intelbras.AuthState{ClientUUID: id, AccessToken: jwt(id), ExpiresAt: time.Now().Add(time.Hour)}
				if outcome == "login-failure" {
					return errors.New("private auth response")
				}
				if outcome == "account-mismatch" {
					payload, _ := json.Marshal(map[string]string{"cli-id": "003-4077-" + id, "qv-uid": "999999"})
					a.AccessToken = "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
				}
				return save(a)
			}}, alarm: fakeAlarm(func(_ context.Context, _ intelbras.Config, _ intelbras.Credentials, id, token string, a *intelbras.AuthState, save func(*intelbras.AuthState) error) error {
				bindings++
				if !a.InfoRegistered || !a.TokenRegistered || a.AlarmSessionID != "retained-session" || token != "own-token" {
					t.Fatal("lost binding progress")
				}
				a.AlarmRegistered = true
				return save(a)
			}), listen: func(_ context.Context, _ receiver.Config, g *receiver.State, _ func(*receiver.State) error, _ func(Event) error, ready func() error) error {
				if g.ClientUUID != google.ClientUUID {
					t.Fatal("lost Google identity")
				}
				return ready()
			}}
			o.Reauthenticate = true
			err := run(context.Background(), o, emit, ready, b)
			if outcome != "success" {
				after, _ := os.ReadFile(path)
				if err == nil || !bytes.Equal(before, after) || bindings != 0 {
					t.Fatal("account mismatch changed state", err)
				}
			} else if err != nil || bindings != 1 {
				t.Fatal(err, bindings)
			}
			if logins != 1 {
				t.Fatal(logins)
			}
		})
	}
}

func TestListenOnlyNeverCreatesOrCompletesState(t *testing.T) {
	o := options(t)
	o.ListenOnly = true
	emit, ready := callbacks()
	if err := run(context.Background(), o, emit, ready, backend{}); err == nil {
		t.Fatal("missing state accepted")
	}
	if _, err := os.Stat(filepath.Join(o.StateDir, "push.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("created receiver identity")
	}
}

func TestListenOnlyRejectsIncompleteStateWithoutNetworkOrIdentityMutation(t *testing.T) {
	for _, kind := range []string{"missing-binding", "missing-google", "wrong-device", "complete"} {
		t.Run(kind, func(t *testing.T) {
			o := options(t)
			o.ListenOnly = true
			o.Email = ""
			o.Password = ""
			var cfg receiver.Config
			json.Unmarshal(appJSON, &cfg)
			cfg.DeviceID = o.DeviceID
			google, _ := receiver.NewState(cfg)
			google.AndroidID = 1
			google.SecurityToken = 2
			google.FCMToken = "saved-token"
			auth := intelbras.AuthState{ClientUUID: google.ClientUUID, AccessToken: jwt(google.ClientUUID), ExpiresAt: time.Now().Add(-time.Hour), InfoRegistered: true, TokenRegistered: true, AlarmRegistered: true}
			if kind == "missing-binding" {
				auth.AlarmRegistered = false
			}
			if kind == "missing-google" {
				google.AndroidID = 0
				google.SecurityToken = 0
			}
			state := savedState{1, o.DeviceID, google, auth}
			path := filepath.Join(o.StateDir, "push.json")
			if err := savePrivate(path, &state); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(path)
			if kind == "wrong-device" {
				o.DeviceID = "other-device"
			}
			listens := 0
			b := backend{listen: func(_ context.Context, _ receiver.Config, _ *receiver.State, _ func(*receiver.State) error, _ func(Event) error, ready func() error) error {
				listens++
				return ready()
			}}
			emit, ready := callbacks()
			err := run(context.Background(), o, emit, ready, b)
			if kind == "complete" {
				if err != nil || listens != 1 {
					t.Fatal(err, listens)
				}
			} else if err == nil || listens != 0 {
				t.Fatal("unsafe state listened", err, listens)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("listen-only altered receiver state")
			}
		})
	}
}
