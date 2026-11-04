package intelbras

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(body string) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

const uuid = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

func config() Config {
	return Config{UserID: "123456", RegionID: "1", Language: "pt", Timezone: "gmt-03:00", AppVersion: "2.4.77.17", SystemVersion: "13"}
}
func TestLoginThenBindNativeWireAndPersistence(t *testing.T) {
	var calls, saves int
	password := strings.Repeat("a", 64)
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		switch calls {
		case 1:
			if r.Method != "GET" || r.URL.Host != oauthHost+":443" || r.URL.Path != "/qvoauthv2/token" {
				t.Fatal("wrong OAuth route")
			}
			q := r.URL.Query()
			h := sha256.Sum256([]byte(password))
			want := map[string]string{"grant_type": "password", "client_id": "003-4077-" + uuid, "client_type": "3", "oemid": "A0077,G0077", "appid": "4077", "usr": "test@example.invalid", "pwd": hex.EncodeToString(h[:]), "region_id": "1", "client_flag": "1"}
			if len(q) != len(want) {
				t.Fatal("unexpected OAuth query fields")
			}
			for k, v := range want {
				if q.Get(k) != v {
					t.Errorf("wrong query field %s", k)
				}
			}
			return response(`{"access_token":"access-private","refresh_token":"refresh-private","expires_in":3600}`), nil
		case 2, 3:
			if r.Method != "POST" || r.URL.Host != openAPIHost+":443" || r.Header.Get("token") != "access-private" || r.Header.Get("Authorization") != "" {
				t.Fatal("wrong binding route or authentication")
			}
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				t.Fatal("invalid request JSON")
			}
			if body["clientId"] != "003-4077-"+uuid || body["appId"] != "4077" {
				t.Fatal("wrong own client identity")
			}
			if calls == 2 {
				if saves != 1 || r.URL.Path != "/openapi-tdk/client/info" {
					t.Fatal("tokens not persisted before info")
				}
				expected := map[string]string{"clientId": "003-4077-" + uuid, "appId": "4077", "oemId": "A0077,G0077", "userId": "123456", "systemType": "android", "systemVersion": "13", "clientType": "3", "appVersion": "2.4.77.17", "voipEnable": "0", "language": "pt", "timezone": "gmt-03:00", "pushType": "fcm"}
				if len(body) != len(expected) {
					t.Fatal("unexpected info fields")
				}
				for k, v := range expected {
					if body[k] != v {
						t.Errorf("wrong info field %s", k)
					}
				}
			} else {
				if saves != 2 || r.URL.Path != "/openapi-tdk/client/push/token" {
					t.Fatal("info not persisted before token")
				}
				entries, ok := body["serverTypeToken"].([]any)
				if !ok || len(entries) != 1 {
					t.Fatal("wrong token list")
				}
				entry := entries[0].(map[string]any)
				if entry["serverType"] != "fcm" || entry["pushToken"] != "own-fcm" || len(entry) != 2 || len(body) != 3 {
					t.Fatal("wrong FCM payload")
				}
			}
			return response(`{"code":0}`), nil
		default:
			t.Fatal("unexpected retry")
			return nil, nil
		}
	})}
	c := &Client{oauth: client, openAPI: client}
	state := &AuthState{}
	save := func(s *AuthState) error {
		saves++
		if saves == 1 && (s.ClientUUID != uuid || s.RefreshToken != "refresh-private" || s.ExpiresAt.Before(time.Now())) {
			t.Fatal("OAuth state missing")
		}
		return nil
	}
	if err := c.Login(context.Background(), config(), Credentials{"test@example.invalid", password}, uuid, state, save); err != nil {
		t.Fatal(err)
	}
	if err := c.Bind(context.Background(), config(), uuid, "own-fcm", state, save); err != nil {
		t.Fatal(err)
	}
	if calls != 3 || saves != 3 || !state.InfoRegistered || !state.TokenRegistered {
		t.Fatal("incorrect final stages")
	}
}
func TestFailureStopsWithoutLeaks(t *testing.T) {
	for _, body := range []string{`{"code":5,"msg":"secret-token-and-password"}`, `{"msg":"secret-token-and-password"}`, `secret-token-and-password`} {
		t.Run(body[:1]+string(rune(len(body))), func(t *testing.T) {
			calls := 0
			c := &Client{openAPI: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { calls++; return response(body), nil })}}
			state := &AuthState{AccessToken: "secret-access", ClientUUID: uuid}
			err := c.Bind(context.Background(), config(), uuid, "own-fcm", state, func(*AuthState) error { return nil })
			if err == nil || calls != 1 || strings.Contains(err.Error(), "secret") || state.InfoRegistered || state.TokenRegistered {
				t.Fatal("failure did not stop safely")
			}
		})
	}
}
func TestPersistenceFailureStopsNextRequest(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return response(`{"access_token":"a","refresh_token":"b"}`), nil
	})}
	c := &Client{oauth: client}
	state := &AuthState{}
	err := c.Login(context.Background(), config(), Credentials{"email", "password"}, uuid, state, func(*AuthState) error { return errors.New("private file secret") })
	if err == nil || strings.Contains(err.Error(), "secret") || calls != 1 || state.AccessToken != "a" {
		t.Fatal("persistence failure lost tokens or leaked details")
	}
	c.openAPI = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return response(`{"code":0}`), nil })}
	err = c.Bind(context.Background(), config(), uuid, "fcm", state, func(*AuthState) error { return errors.New("secret") })
	if err == nil || calls != 2 || !state.InfoRegistered || state.TokenRegistered {
		t.Fatal("did not stop after info save failure")
	}
}
func TestInvalidInputNeverCallsTransport(t *testing.T) {
	c := &Client{openAPI: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("unexpected network"); return nil, nil })}}
	for _, tc := range []struct {
		id, user, token string
		expired         bool
	}{{"phone-identity", "123", "fcm", false}, {uuid, "email@example.invalid", "fcm", false}, {uuid, "123", "", false}, {uuid, "123", "fcm", true}} {
		cfg := config()
		cfg.UserID = tc.user
		state := &AuthState{AccessToken: "secret", ClientUUID: tc.id}
		if tc.expired {
			state.ExpiresAt = time.Now().Add(-time.Minute)
		}
		if c.Bind(context.Background(), cfg, tc.id, tc.token, state, func(*AuthState) error { return nil }) == nil {
			t.Fatal("accepted invalid input")
		}
	}
	state := &AuthState{AccessToken: "secret", ClientUUID: strings.Repeat("b", 32)}
	if c.Bind(context.Background(), config(), uuid, "fcm", state, func(*AuthState) error { return nil }) == nil {
		t.Fatal("accepted different identity")
	}
}
func TestPinnedTLSAndRedirectPolicy(t *testing.T) {
	if _, err := NewClient(&tls.Config{InsecureSkipVerify: true}, &tls.Config{}, []byte("pin"), []byte("pin")); err == nil {
		t.Fatal("accepted insecure TLS")
	}
	base := &tls.Config{ServerName: "wrong.invalid", MinVersion: tls.VersionTLS10}
	c, err := NewClient(base, &tls.Config{}, hashBytes("oauth-pin"), hashBytes("api-pin"))
	if err != nil {
		t.Fatal(err)
	}
	if base.ServerName != "wrong.invalid" || base.MinVersion != tls.VersionTLS10 {
		t.Fatal("mutated caller TLS")
	}
	for _, tc := range []struct {
		client    *http.Client
		host, pin string
	}{{c.oauth, oauthHost, "oauth-pin"}, {c.openAPI, openAPIHost, "api-pin"}} {
		transport := tc.client.Transport.(*http.Transport)
		cfg := transport.TLSClientConfig
		if cfg.InsecureSkipVerify || cfg.ServerName != tc.host || cfg.MinVersion < tls.VersionTLS12 || transport.Proxy != nil || tc.client.Timeout != 15*time.Second {
			t.Fatal("TLS or transport weakened")
		}
		cert := &x509.Certificate{Raw: []byte(tc.pin)}
		valid := tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}
		if cfg.VerifyConnection(valid) != nil {
			t.Fatal("valid leaf rejected")
		}
		valid.PeerCertificates = []*x509.Certificate{{Raw: []byte("other")}}
		if cfg.VerifyConnection(valid) == nil {
			t.Fatal("wrong leaf accepted")
		}
		valid.PeerCertificates = []*x509.Certificate{cert}
		valid.VerifiedChains = nil
		if cfg.VerifyConnection(valid) == nil {
			t.Fatal("unverified chain accepted")
		}
		if tc.client.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
			t.Fatal("redirect allowed")
		}
	}
}
func TestTransportAndResponseErrorsSanitized(t *testing.T) {
	for _, tc := range []struct {
		response *http.Response
		err      error
	}{{nil, errors.New("https://private.invalid/?pwd=secret")}, {&http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader("secret"))}, nil}, {response(strings.Repeat("s", maxResponse+1)), nil}} {
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return tc.response, tc.err })}
		var result any
		err := request(context.Background(), client, "GET", "https://"+oauthHost+":443/qvoauthv2/token?pwd=secret", "", nil, &result)
		if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private") {
			t.Fatal("unsafe request error")
		}
	}
}

func hashBytes(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }

func TestOAuthAccountClaims(t *testing.T) {
	for _, tc := range []struct {
		client, user string
		good         bool
	}{
		{"003-4077-" + uuid, `"123456"`, true},
		{"003-4077-" + uuid, `123456`, true},
		{"003-4077-" + uuid, `"email@example.invalid"`, false},
		{"003-4077-" + uuid, `123.5`, false},
		{"003-4077-other", `"123456"`, false},
	} {
		payload := `{"cli-id":"` + tc.client + `","qv-uid":` + tc.user + `}`
		token := "header." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".signature"
		s := AuthState{ClientUUID: uuid, AccessToken: token}
		id, err := s.AccountID(uuid)
		if (err == nil) != tc.good || (tc.good && id != "123456") {
			t.Fatal("invalid claim outcome", err)
		}
	}
}

func TestBindResumesOnlyMissingPhase(t *testing.T) {
	calls := 0
	c := &Client{openAPI: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path != "/openapi-tdk/client/push/token" {
			t.Fatal("completed info phase repeated")
		}
		return response(`{"code":0}`), nil
	})}}
	s := &AuthState{ClientUUID: uuid, AccessToken: "synthetic", InfoRegistered: true}
	if err := c.Bind(context.Background(), config(), uuid, "own-fcm", s, func(*AuthState) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := c.Bind(context.Background(), config(), uuid, "own-fcm", s, func(*AuthState) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !s.TokenRegistered {
		t.Fatal("token phase repeated")
	}
}
