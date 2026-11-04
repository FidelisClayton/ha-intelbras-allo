// Package intelbras binds an independent receiver identity to Intelbras push.
package intelbras

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const oauthHost = "oauth2intelbras.qvcloud.net"
const openAPIHost = "tdkopenapiintelbras.qvcloud.net"
const maxResponse = 2 << 20

type Config struct {
	UserID        string `json:"user_id"`
	RegionID      string `json:"region_id"`
	Language      string `json:"language"`
	Timezone      string `json:"timezone"`
	AppVersion    string `json:"app_version"`
	SystemVersion string `json:"system_version"`
}
type Credentials struct{ Email, Password string }
type AuthState struct {
	AlarmRegistered bool      `json:"alarm_registered"`
	AlarmSessionID  string    `json:"alarm_session_id"`
	AccessToken     string    `json:"access_token"`
	RefreshToken    string    `json:"refresh_token"`
	ExpiresAt       time.Time `json:"expires_at"`
	ClientUUID      string    `json:"client_uuid"`
	InfoRegistered  bool      `json:"info_registered"`
	TokenRegistered bool      `json:"token_registered"`
}
type Client struct{ oauth, openAPI *http.Client }

// NewClient adds an exact leaf pin to normal CA and DNS validation. Pins are SHA-256 hashes of the DER leaf certificate.
func NewClient(oauthTLS, openAPITLS *tls.Config, oauthLeaf, openAPILeaf []byte) (*Client, error) {
	oauth, err := pinnedClient(oauthTLS, oauthHost, oauthLeaf)
	if err != nil {
		return nil, err
	}
	api, err := pinnedClient(openAPITLS, openAPIHost, openAPILeaf)
	if err != nil {
		return nil, err
	}
	return &Client{oauth: oauth, openAPI: api}, nil
}
func pinnedClient(base *tls.Config, host string, leaf []byte) (*http.Client, error) {
	if base == nil || base.InsecureSkipVerify || len(leaf) != sha256.Size {
		return nil, errors.New("invalid Intelbras TLS configuration")
	}
	cfg := base.Clone()
	cfg.Certificates = nil
	cfg.GetClientCertificate = nil
	cfg.ServerName = host
	if cfg.MinVersion < tls.VersionTLS12 {
		cfg.MinVersion = tls.VersionTLS12
	}
	pin := append([]byte(nil), leaf...)
	previous := cfg.VerifyConnection
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		if len(cs.VerifiedChains) == 0 || len(cs.PeerCertificates) == 0 || !matchesPin(cs.PeerCertificates[0].Raw, pin) {
			return errors.New("Intelbras TLS verification failed")
		}
		if previous != nil {
			return previous(cs)
		}
		return nil
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}
func bounded(s string, max int) bool {
	return s != "" && len(s) <= max && !strings.ContainsAny(s, "\r\n\x00")
}
func identity(id string) bool {
	return regexp.MustCompile(`^(?:[0-9a-f]{32}|[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12})$`).MatchString(id)
}
func digits(s string) bool { return len(s) <= 32 && regexp.MustCompile(`^[0-9]+$`).MatchString(s) }
func persist(state *AuthState, save func(*AuthState) error) error {
	if err := save(state); err != nil {
		return errors.New("cannot save Intelbras authentication state")
	}
	return nil
}

// Login always hashes the raw password, including a raw 64-character password.
// OAuth is persisted before any binding request. There are no retries or refreshes.
func (c *Client) Login(ctx context.Context, cfg Config, cred Credentials, clientUUID string, state *AuthState, save func(*AuthState) error) error {
	if c == nil || c.oauth == nil || state == nil || save == nil || !identity(clientUUID) || !digits(cfg.RegionID) || !bounded(cred.Email, 1024) || !bounded(cred.Password, 4096) {
		return errors.New("invalid Intelbras login input")
	}
	hash := sha256.Sum256([]byte(cred.Password))
	q := url.Values{"grant_type": {"password"}, "client_id": {"003-4077-" + clientUUID}, "client_type": {"3"}, "oemid": {"A0077,G0077"}, "appid": {"4077"}, "usr": {cred.Email}, "pwd": {hex.EncodeToString(hash[:])}, "region_id": {cfg.RegionID}, "client_flag": {"1"}}
	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := request(ctx, c.oauth, http.MethodGet, "https://"+oauthHost+":443/qvoauthv2/token?"+q.Encode(), "", nil, &result); err != nil {
		return err
	}
	if !bounded(result.AccessToken, 16384) || !bounded(result.RefreshToken, 16384) || result.ExpiresIn < 0 || result.ExpiresIn > 31536000 {
		return errors.New("invalid Intelbras OAuth response")
	}
	next := AuthState{AccessToken: result.AccessToken, RefreshToken: result.RefreshToken, ClientUUID: clientUUID}
	if result.ExpiresIn > 0 {
		next.ExpiresAt = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)
	}
	*state = next
	return persist(state, save)
}

// Bind registers only the supplied receiver UUID and FCM token, in native request order.
func (c *Client) Bind(ctx context.Context, cfg Config, clientUUID, fcmToken string, state *AuthState, save func(*AuthState) error) error {
	if c == nil || c.openAPI == nil || state == nil || save == nil || !identity(clientUUID) || state.ClientUUID != clientUUID || !bounded(state.AccessToken, 16384) || !bounded(fcmToken, 16384) || !digits(cfg.UserID) {
		return errors.New("invalid Intelbras binding input")
	}
	for _, v := range []string{cfg.Language, cfg.Timezone, cfg.AppVersion, cfg.SystemVersion} {
		if !bounded(v, 128) {
			return errors.New("invalid Intelbras binding configuration")
		}
	}
	if !state.ExpiresAt.IsZero() && !time.Now().Before(state.ExpiresAt) {
		return errors.New("Intelbras OAuth token expired")
	}
	// Native PushRegisterInfoReq fields; systemVersion describes this receiver profile.
	info := map[string]string{"clientId": "003-4077-" + clientUUID, "appId": "4077", "oemId": "A0077,G0077", "userId": cfg.UserID, "systemType": "android", "systemVersion": cfg.SystemVersion, "clientType": "3", "appVersion": cfg.AppVersion, "voipEnable": "0", "language": cfg.Language, "timezone": cfg.Timezone, "pushType": "fcm"}
	if !state.InfoRegistered {
		if err := c.post(ctx, "/openapi-tdk/client/info", state.AccessToken, info); err != nil {
			return err
		}
		state.InfoRegistered = true
		if err := persist(state, save); err != nil {
			return err
		}
	}
	if state.TokenRegistered {
		return nil
	}
	token := struct {
		AppID           string              `json:"appId"`
		ClientID        string              `json:"clientId"`
		ServerTypeToken []map[string]string `json:"serverTypeToken"`
	}{"4077", "003-4077-" + clientUUID, []map[string]string{{"serverType": "fcm", "pushToken": fcmToken}}}
	if err := c.post(ctx, "/openapi-tdk/client/push/token", state.AccessToken, token); err != nil {
		return err
	}
	state.TokenRegistered = true
	return persist(state, save)
}
func (c *Client) post(ctx context.Context, path, token string, body any) error {
	var result struct {
		Code *int `json:"code"`
	}
	if err := request(ctx, c.openAPI, http.MethodPost, "https://"+openAPIHost+":443"+path, token, body, &result); err != nil {
		return err
	}
	// QvBaseOAuth2Manager maps successful OpenAPI responses from code 0.
	if result.Code == nil {
		return errors.New("missing Intelbras result code")
	}
	if *result.Code != 0 {
		return fmt.Errorf("Intelbras result code %d", *result.Code)
	}
	return nil
}
func request(ctx context.Context, client *http.Client, method, target, token string, body, result any) error {
	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil || len(payload) > 32768 {
			return errors.New("invalid Intelbras request")
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(payload))
	if err != nil {
		return errors.New("invalid Intelbras request")
	}
	if token != "" {
		req.Header.Set("token", token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("Intelbras request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Intelbras HTTP status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil || len(data) > maxResponse {
		return errors.New("invalid Intelbras response")
	}
	if json.Unmarshal(data, result) != nil {
		return errors.New("invalid Intelbras response")
	}
	return nil
}

func matchesPin(raw, pin []byte) bool { h := sha256.Sum256(raw); return bytes.Equal(h[:], pin) }

// AccountID reads only the token returned over the verified OAuth connection.
// The JWT is not used as independent signature evidence; TLS authenticates its source.
func (s *AuthState) AccountID(clientUUID string) (string, error) {
	if s == nil || s.ClientUUID != clientUUID || !identity(clientUUID) || !bounded(s.AccessToken, 16384) {
		return "", errors.New("invalid OAuth identity")
	}
	parts := strings.Split(s.AccessToken, ".")
	if len(parts) != 3 {
		return "", errors.New("invalid OAuth claims")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", errors.New("invalid OAuth claims")
	}
	var claims struct {
		ClientID string          `json:"cli-id"`
		UserID   json.RawMessage `json:"qv-uid"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.ClientID != "003-4077-"+clientUUID {
		return "", errors.New("OAuth client identity mismatch")
	}
	id := string(claims.UserID)
	if len(id) > 0 && id[0] == '"' {
		if json.Unmarshal(claims.UserID, &id) != nil {
			return "", errors.New("invalid OAuth account identity")
		}
	}
	if !digits(id) {
		return "", errors.New("invalid OAuth account identity")
	}
	return id, nil
}
