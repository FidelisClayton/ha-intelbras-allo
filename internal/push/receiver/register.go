// Package receiver implements a bounded, independent Google push identity.
package receiver

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/golang/protobuf/proto"
	fcm "github.com/morhaviv/go-fcm-receiver"
)

type Config struct {
	ProjectID       string `json:"project_id"`
	AppID           string `json:"app_id"`
	APIKey          string `json:"api_key"`
	SenderID        string `json:"sender_id"`
	PackageName     string `json:"package_name"`
	PackageCertSHA1 string `json:"package_cert_sha1"`
	AppVersionCode  string `json:"app_version_code"`
	AppVersionName  string `json:"app_version_name"`
	DeviceID        string `json:"device_id"`
	// Optional metadata must describe this client, never an unrelated phone.
	FirebaseClient string `json:"firebase_client,omitempty"`
	OSVersion      string `json:"os_version,omitempty"`
	GMSVersion     string `json:"gms_version,omitempty"`
	ClientVersion  string `json:"client_version,omitempty"`
	TargetVersion  string `json:"target_version,omitempty"`
}

type State struct {
	FID                      string   `json:"fid"`
	ClientUUID               string   `json:"client_uuid"`
	AndroidID                uint64   `json:"android_id"`
	SecurityToken            uint64   `json:"security_token"`
	InstallationAuthToken    string   `json:"installation_auth_token"`
	InstallationRefreshToken string   `json:"installation_refresh_token"`
	FCMToken                 string   `json:"fcm_token"`
	PersistentIDs            []string `json:"persistent_ids"`
}

func (c Config) Validate() error {
	for _, v := range []string{c.ProjectID, c.AppID, c.APIKey, c.SenderID, c.PackageName, c.PackageCertSHA1, c.AppVersionCode, c.AppVersionName, c.DeviceID} {
		if v == "" || len(v) > 1024 || strings.ContainsAny(v, "\r\n\x00") {
			return errors.New("invalid push configuration")
		}
	}
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`).MatchString(c.ProjectID) {
		return errors.New("invalid push project")
	}
	cert, err := hex.DecodeString(c.PackageCertSHA1)
	if err != nil || len(cert) != 20 {
		return errors.New("invalid package certificate fingerprint")
	}
	for _, v := range []string{c.FirebaseClient, c.OSVersion, c.GMSVersion, c.ClientVersion, c.TargetVersion} {
		if len(v) > 4096 || strings.ContainsAny(v, "\r\n\x00") {
			return errors.New("invalid optional push metadata")
		}
	}
	return nil
}

func NewState(c Config) (*State, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	var b [17]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, errors.New("identity generation failed")
	}
	b[0] = (b[0] & 15) | 112
	var u [16]byte
	if _, err := rand.Read(u[:]); err != nil {
		return nil, errors.New("identity generation failed")
	}
	u[6] = (u[6] & 15) | 64
	u[8] = (u[8] & 63) | 128
	h := hex.EncodeToString(u[:])
	return &State{FID: base64.RawURLEncoding.EncodeToString(b[:])[:22], ClientUUID: h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]}, nil
}

func ValidState(s *State) bool {
	if s == nil {
		return false
	}
	for _, id := range s.PersistentIDs {
		if len(id) > 4096 {
			return false
		}
	}
	for _, v := range []string{s.InstallationAuthToken, s.InstallationRefreshToken, s.FCMToken} {
		if len(v) > 16384 || strings.ContainsAny(v, "\r\n\x00") {
			return false
		}
	}
	return regexp.MustCompile(`^[cdef][A-Za-z0-9_-]{21}$`).MatchString(s.FID) && regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(s.ClientUUID) && len(s.PersistentIDs) <= 1024 && ((s.AndroidID == 0) == (s.SecurityToken == 0))
}

func googleClient() *http.Client {
	return &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, Proxy: nil}}
}

func request(ctx context.Context, client *http.Client, endpoint string, body []byte, headers map[string]string) ([]byte, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || (u.Host != "android.clients.google.com" && u.Host != "firebaseinstallations.googleapis.com") {
		return nil, errors.New("push endpoint refused")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("push request invalid")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("Google push request failed")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil || len(b) > 2<<20 {
		return nil, errors.New("Google push response invalid")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errors.New("Google push request rejected")
	}
	return b, nil
}

func Register(ctx context.Context, c Config, s *State, save func(*State) error) error {
	return register(ctx, googleClient(), c, s, save)
}
func register(ctx context.Context, client *http.Client, c Config, s *State, save func(*State) error) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if !ValidState(s) || save == nil {
		return errors.New("invalid push state or persistence callback")
	}
	if s.InstallationAuthToken == "" {
		body, _ := json.Marshal(map[string]string{"fid": s.FID, "appId": c.AppID, "authVersion": "FIS_v2", "sdkVersion": "a:17.0.0"})
		headers := map[string]string{"Content-Type": "application/json", "Accept": "application/json", "x-goog-api-key": c.APIKey, "X-Android-Package": c.PackageName, "X-Android-Cert": c.PackageCertSHA1}
		if c.FirebaseClient != "" {
			headers["x-firebase-client"] = c.FirebaseClient
			headers["x-firebase-client-log-type"] = "3"
		}
		b, err := request(ctx, client, "https://firebaseinstallations.googleapis.com/v1/projects/"+c.ProjectID+"/installations", body, headers)
		if err != nil {
			return err
		}
		var r struct {
			FID          string `json:"fid"`
			RefreshToken string `json:"refreshToken"`
			AuthToken    struct {
				Token string `json:"token"`
			} `json:"authToken"`
		}
		if json.Unmarshal(b, &r) != nil || r.AuthToken.Token == "" || r.RefreshToken == "" || (r.FID != "" && r.FID != s.FID) {
			return errors.New("installation response invalid")
		}
		s.InstallationAuthToken = r.AuthToken.Token
		s.InstallationRefreshToken = r.RefreshToken
		if err := save(s); err != nil {
			return errors.New("push state save failed")
		}
	}
	if s.AndroidID == 0 {
		id := int64(0)
		token := uint64(0)
		body, err := proto.Marshal(fcm.CreateCheckInRequest(&id, &token))
		if err != nil {
			return errors.New("checkin encoding failed")
		}
		b, err := request(ctx, client, fcm.CheckInUrl, body, map[string]string{"Content-Type": "application/x-protobuf", "User-Agent": ""})
		if err != nil {
			return err
		}
		var r fcm.AndroidCheckinResponse
		if proto.Unmarshal(b, &r) != nil || r.GetAndroidId() == 0 || r.GetSecurityToken() == 0 {
			return errors.New("checkin response invalid")
		}
		s.AndroidID = r.GetAndroidId()
		s.SecurityToken = r.GetSecurityToken()
		if err := save(s); err != nil {
			return errors.New("push state save failed")
		}
	}
	if s.FCMToken == "" {
		v := url.Values{"X-subtype": {c.SenderID}, "device": {strconv.FormatUint(s.AndroidID, 10)}, "app": {c.PackageName}, "cert": {c.PackageCertSHA1}, "app_ver": {c.AppVersionCode}, "X-app_ver": {c.AppVersionCode}, "X-app_ver_name": {c.AppVersionName}, "X-scope": {"*"}, "X-Goog-Firebase-Installations-Auth": {s.InstallationAuthToken}, "X-gms_app_id": {c.AppID}, "sender": {c.SenderID}}
		for k, value := range map[string]string{"X-osv": c.OSVersion, "X-cliv": c.ClientVersion, "X-gmsv": c.GMSVersion, "X-Firebase-Client": c.FirebaseClient, "target_ver": c.TargetVersion} {
			if value != "" {
				v.Set(k, value)
			}
		}
		if c.FirebaseClient != "" {
			v.Set("X-Firebase-Client-Log-Type", "1")
		}
		b, err := request(ctx, client, fcm.RegisterUrl, []byte(v.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Authorization": "AidLogin " + strconv.FormatUint(s.AndroidID, 10) + ":" + strconv.FormatUint(s.SecurityToken, 10), "User-Agent": ""})
		if err != nil {
			return err
		}
		r, err := url.ParseQuery(string(b))
		if err != nil || r.Get("Error") != "" || r.Get("token") == "" {
			return errors.New("sender registration rejected")
		}
		s.FCMToken = r.Get("token")
		if err := save(s); err != nil {
			return errors.New("push state save failed")
		}
	}
	return nil
}
