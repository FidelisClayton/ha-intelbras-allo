// Package push receives filtered physical bell events using this installation's own FCM identity.
package push

import (
	"context"
	"crypto/tls"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/FidelisClayton/ha-intelbras-allo/internal/bootstrap"
	"github.com/FidelisClayton/ha-intelbras-allo/internal/push/intelbras"
	"github.com/FidelisClayton/ha-intelbras-allo/internal/push/receiver"
)

//go:embed app.json
var appJSON []byte

// Options contains private runtime inputs. Password is never persisted.
type Options struct {
	DeviceID, Email, Password, StateDir string
	Reauthenticate, ListenOnly          bool
}

// Event contains only the selected device's bell fields, never the raw alarm payload.
type Event = receiver.Event

// Error exposes fixed stage/code values without remote response bodies or credentials.
type Error struct{ Stage, Code string }

func (e *Error) Error() string         { return e.Stage + ":" + e.Code }
func failure(stage, code string) error { return &Error{Stage: stage, Code: code} }

type savedState struct {
	Version   int                 `json:"version"`
	DeviceID  string              `json:"device_id"`
	Google    *receiver.State     `json:"google"`
	Intelbras intelbras.AuthState `json:"intelbras"`
}
type accountAPI interface {
	Login(context.Context, intelbras.Config, intelbras.Credentials, string, *intelbras.AuthState, func(*intelbras.AuthState) error) error
	Bind(context.Context, intelbras.Config, string, string, *intelbras.AuthState, func(*intelbras.AuthState) error) error
}
type alarmAPI interface {
	Register(context.Context, intelbras.Config, intelbras.Credentials, string, string, *intelbras.AuthState, func(*intelbras.AuthState) error) error
}
type backend struct {
	register func(context.Context, receiver.Config, *receiver.State, func(*receiver.State) error) error
	account  accountAPI
	alarm    alarmAPI
	listen   func(context.Context, receiver.Config, *receiver.State, func(*receiver.State) error, func(Event) error, func() error) error
}

// Run resumes persisted progress, completes missing bindings once, then blocks in MCS.
// Run never retries; OAuth renewal requires the explicit Reauthenticate option.
// onReady is called only after all bindings and an accepted Google LoginResponse.
func Run(ctx context.Context, options Options, onEvent func(Event) error, onReady func() error) error {
	pin, _ := hex.DecodeString("f35f1c7d67e7c32e439e11e15a189ffda7664b3d30747a51a795b279dd742f87")
	account, err := intelbras.NewClient(&tls.Config{}, &tls.Config{}, pin, pin)
	if err != nil {
		return failure("trust", "invalid")
	}
	pin, _ = hex.DecodeString("e7167be07fc554614ce611b1be054c29cfc39926695d6a6717c9e631dc83276a")
	alarm, err := intelbras.NewAlarmClient(bootstrap.VendorCA(), pin)
	if err != nil {
		return failure("trust", "invalid")
	}
	return run(ctx, options, onEvent, onReady, backend{receiver.Register, account, alarm, receiver.Listen})
}

func run(ctx context.Context, o Options, onEvent func(Event) error, onReady func() error, b backend) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if onEvent == nil || onReady == nil || o.StateDir == "" || !filepath.IsAbs(o.StateDir) {
		return failure("input", "invalid")
	}
	var cfg receiver.Config
	if json.Unmarshal(appJSON, &cfg) != nil {
		return failure("configuration", "invalid")
	}
	cfg.DeviceID = o.DeviceID
	if cfg.Validate() != nil || len(o.Email) > 1024 || len(o.Password) > 4096 || strings.ContainsAny(o.Email+o.Password, "\r\n\x00") {
		return failure("input", "invalid")
	}
	path := filepath.Join(o.StateDir, "push.json")
	if _, err := privateParent(path, true); err != nil {
		return failure("state", "unsafe")
	}
	lock, err := lockState(filepath.Join(o.StateDir, "push.lock"))
	if err != nil {
		return failure("state", "locked_or_unsafe")
	}
	defer lock.Close()
	var state savedState
	err = readPrivate(path, 1<<20, &state)
	if errors.Is(err, os.ErrNotExist) {
		if o.ListenOnly {
			return failure("state", "missing")
		}
		google, err := receiver.NewState(cfg)
		if err != nil {
			return failure("identity", "generation_failed")
		}
		state = savedState{Version: 1, DeviceID: o.DeviceID, Google: google}
		if savePrivate(path, &state) != nil {
			return failure("identity", "save_failed")
		}
	} else if err != nil {
		return failure("state", "invalid")
	}
	if state.Version != 1 || state.DeviceID != o.DeviceID || !receiver.ValidState(state.Google) {
		return failure("state", "identity_mismatch")
	}
	auth := &state.Intelbras
	if (auth.AccessToken != "" && auth.ClientUUID != state.Google.ClientUUID) || (auth.AlarmRegistered && (!auth.InfoRegistered || !auth.TokenRegistered)) || (auth.TokenRegistered && !auth.InfoRegistered) {
		return failure("state", "identity_mismatch")
	}
	if o.ListenOnly && (state.Google.AndroidID == 0 || state.Google.FCMToken == "" || auth.AccessToken == "" || !auth.InfoRegistered || !auth.TokenRegistered || !auth.AlarmRegistered) {
		return failure("bindings", "incomplete")
	}
	if auth.AccessToken != "" {
		if _, err := auth.AccountID(state.Google.ClientUUID); err != nil {
			return failure("oauth_login", "identity_invalid")
		}
	}
	if !o.Reauthenticate && (!auth.InfoRegistered || !auth.TokenRegistered || !auth.AlarmRegistered) && !auth.ExpiresAt.IsZero() && !time.Now().Before(auth.ExpiresAt) {
		return failure("oauth_login", "expired")
	}
	saveGoogle := func(s *receiver.State) error { state.Google = s; return savePrivate(path, &state) }
	saveAuth := func(s *intelbras.AuthState) error { state.Intelbras = *s; return savePrivate(path, &state) }
	setup, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	finish := func(stage string, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if setup.Err() != nil {
			return failure(stage, "timeout")
		}
		if err != nil {
			return failure(stage, "failed")
		}
		return nil
	}
	if o.Reauthenticate && (o.ListenOnly || o.Email == "" || o.Password == "") {
		return failure("oauth_login", "credentials_required")
	}
	if o.Email == "" || o.Password == "" {
		if auth.AccessToken == "" {
			return failure("oauth_login", "credentials_required")
		}
		if !auth.AlarmRegistered {
			return failure("native_binding", "credentials_required")
		}
	}
	if state.Google.FCMToken == "" {
		if err := b.register(setup, cfg, state.Google, saveGoogle); err != nil {
			return finish("google_registration", err)
		}
	}
	icfg := intelbras.Config{RegionID: "1", Language: "pt", Timezone: timezone(time.Now()), AppVersion: cfg.AppVersionName, SystemVersion: "11"}
	credentials := intelbras.Credentials{Email: o.Email, Password: o.Password}
	if auth.AccessToken == "" || o.Reauthenticate {
		previous := *auth
		candidate := previous
		saveLogin := func(next *intelbras.AuthState) error {
			account, err := next.AccountID(state.Google.ClientUUID)
			if err != nil {
				return errors.New("invalid OAuth identity")
			}
			if previous.AccessToken != "" {
				trusted, err := previous.AccountID(state.Google.ClientUUID)
				if err != nil || trusted != account {
					return errors.New("OAuth account mismatch")
				}
			}
			next.InfoRegistered, next.TokenRegistered = previous.InfoRegistered, previous.TokenRegistered
			next.AlarmRegistered, next.AlarmSessionID = previous.AlarmRegistered, previous.AlarmSessionID
			return saveAuth(next)
		}
		if err := b.account.Login(setup, icfg, credentials, state.Google.ClientUUID, &candidate, saveLogin); err != nil {
			return finish("oauth_login", err)
		}
	}
	if (!auth.InfoRegistered || !auth.TokenRegistered || !auth.AlarmRegistered) && !auth.ExpiresAt.IsZero() && !time.Now().Before(auth.ExpiresAt) {
		return failure("oauth_login", "expired")
	}
	accountID, err := auth.AccountID(state.Google.ClientUUID)
	if err != nil {
		return failure("oauth_login", "identity_invalid")
	}
	icfg.UserID = accountID
	if !auth.InfoRegistered || !auth.TokenRegistered {
		if err := b.account.Bind(setup, icfg, state.Google.ClientUUID, state.Google.FCMToken, auth, saveAuth); err != nil {
			return finish("modern_binding", err)
		}
	}
	if !auth.AlarmRegistered {
		if err := b.alarm.Register(setup, icfg, credentials, state.Google.ClientUUID, state.Google.FCMToken, auth, saveAuth); err != nil {
			return finish("native_binding", err)
		}
	}
	if !auth.InfoRegistered || !auth.TokenRegistered || !auth.AlarmRegistered {
		return failure("bindings", "incomplete")
	}
	cancel()
	err = b.listen(ctx, cfg, state.Google, saveGoogle, onEvent, onReady)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return failure("listen", "failed")
	}
	return nil
}

func timezone(now time.Time) string {
	_, offset := now.Zone()
	sign := "+"
	if offset < 0 {
		sign = "-"
		offset = -offset
	}
	return fmt.Sprintf("gmt%s%02d:%02d", sign, offset/3600, (offset%3600)/60)
}
func lockState(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		f.Close()
		return nil, errors.New("unsafe lock")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
