package intelbras

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"time"
)

const alarmAccountHost = "intelbras-4.qvcloud.net"
const alarmPushHost = "intelbras-1.qvcloud.net"
const alarmAccountURL = "https://" + alarmAccountHost + ":443/auth/user;jus_duplex="
const alarmRegisterURL = "https://" + alarmPushHost + ":4443/UserAlarm"

type AlarmClient struct{ account, alarm *http.Client }

// NewAlarmClient verifies the vendor chain, server-auth usage and exact leaf hash.
// The approved vendor certificate has no DNS names; destinations remain fixed.
func NewAlarmClient(caPEM, leafHash []byte) (*AlarmClient, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("invalid Intelbras alarm CA")
	}
	if len(leafHash) != sha256.Size {
		return nil, errors.New("invalid Intelbras alarm pin")
	}
	pin := append([]byte(nil), leafHash...)
	makeClient := func(host string) *http.Client {
		cfg := &tls.Config{MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS13, ServerName: host, InsecureSkipVerify: true, VerifyConnection: alarmVerifyPeer(roots, pin, host)}
		jar, _ := cookiejar.New(nil)
		return &http.Client{Transport: &http.Transport{TLSClientConfig: cfg, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 15 * time.Second}, Jar: jar, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("Intelbras alarm redirects prohibited") }}
	}
	return &AlarmClient{account: makeClient(alarmAccountHost), alarm: makeClient(alarmPushHost)}, nil
}
func alarmVerifyPeer(roots *x509.CertPool, pin []byte, host string) func(tls.ConnectionState) error {
	return func(cs tls.ConnectionState) error {
		if (host != alarmAccountHost && host != alarmPushHost) || cs.ServerName != host || len(cs.PeerCertificates) == 0 {
			return errors.New("invalid Intelbras alarm TLS peer")
		}
		leaf := cs.PeerCertificates[0]
		intermediates := x509.NewCertPool()
		for _, cert := range cs.PeerCertificates[1:] {
			intermediates.AddCert(cert)
		}
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
			return errors.New("Intelbras alarm TLS chain verification failed")
		}
		if !matchesPin(leaf.Raw, pin) {
			return errors.New("Intelbras alarm TLS pin mismatch")
		}
		return nil
	}
}

type alarmRequest struct {
	XMLName xml.Name `xml:"envelope"`
	Header  struct {
		Flag     string              `xml:"flag"`
		Version  string              `xml:"version,omitempty"`
		Command  string              `xml:"command"`
		Seq      int                 `xml:"seq"`
		Session  string              `xml:"session"`
		UserData *string             `xml:"user-data,omitempty"`
		Client   *alarmAccountClient `xml:"client,omitempty"`
	} `xml:"header"`
	Content any `xml:"content"`
}
type alarmAccountClient struct {
	ID   string `xml:"id"`
	Type int    `xml:"type"`
	OEM  string `xml:"oem"`
	App  string `xml:"app"`
}
type alarmRegistration struct {
	Client struct {
		ID        string `xml:"id"`
		Token     string `xml:"token"`
		TokenType int    `xml:"tokentype"`
		Type      int    `xml:"type"`
		OEM       string `xml:"oemid"`
		App       int    `xml:"appid"`
		Language  string `xml:"notifylang"`
		Timezone  string `xml:"timezone"`
	} `xml:"client"`
	Account struct {
		ID         string `xml:"id"`
		AuthServer string `xml:"authserver"`
	} `xml:"account"`
}
type alarmReply struct {
	XMLName xml.Name `xml:"envelope"`
	Header  struct {
		Result  *int   `xml:"result"`
		Command string `xml:"command"`
		Seq     *int   `xml:"seq"`
		Session struct {
			ID string `xml:"id"`
		} `xml:"session"`
	} `xml:"header"`
	Content struct {
		AccountID string `xml:"account-id"`
	} `xml:"content"`
}

func decodeAlarmReply(data []byte, command string, seq int) (alarmReply, error) {
	var out alarmReply
	if command == "login" {
		// The account down-channel wraps its XML envelope in multipart framing.
		start := bytes.Index(data, []byte("<envelope"))
		if start < 0 {
			return out, errors.New("invalid Intelbras alarm response")
		}
		data = data[start:]
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	if decoder.Decode(&out) != nil || out.Header.Result == nil ||
		(*out.Header.Result != 0 && !(command == "login" && *out.Header.Result == 100)) ||
		(out.Header.Command != "" && out.Header.Command != command) ||
		(out.Header.Seq != nil && *out.Header.Seq != seq) {
		return alarmReply{}, errors.New("invalid Intelbras alarm response")
	}
	return out, nil
}

func alarmPost(ctx context.Context, hc *http.Client, target string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("invalid Intelbras alarm request")
	}
	req.Header.Set("Content-Type", "application/xml;charset=utf-8")
	if target == alarmRegisterURL {
		req.Header.Set("Content-Type", "application/xml")
		req.Header.Set("Charset", "utf-8")
	}
	req.Header.Set("User-Agent", "okhttp/3.12.13")
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("Intelbras alarm request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("Intelbras alarm HTTP request rejected")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil || len(data) > maxResponse {
		return nil, errors.New("invalid Intelbras alarm response")
	}
	return data, nil
}
func encodeAlarmRequest(command, session string, seq int, content any, client *alarmAccountClient) ([]byte, error) {
	var e alarmRequest
	e.Header.Flag = "tdkcloud"
	e.Header.Command = command
	e.Header.Seq = seq
	e.Header.Session = session
	e.Header.Client = client
	e.Content = content
	if client != nil {
		e.Header.Version = "v1.13"
		empty := ""
		e.Header.UserData = &empty
	}
	body, err := xml.Marshal(e)
	if err != nil {
		return nil, errors.New("invalid Intelbras alarm request")
	}
	return append([]byte(xml.Header), body...), nil
}

// Register logs in only this receiver and binds its own FCM token to UserAlarm.
// It neither removes another client nor changes any device alarm settings.
func (c *AlarmClient) Register(ctx context.Context, cfg Config, cred Credentials, clientUUID, fcmToken string, state *AuthState, save func(*AuthState) error) error {
	if c == nil || c.account == nil || c.alarm == nil || state == nil || save == nil || !identity(clientUUID) || state.ClientUUID != clientUUID || !digits(cfg.UserID) || !bounded(cred.Email, 1024) || !bounded(cred.Password, 4096) || !bounded(fcmToken, 16384) || !bounded(cfg.Language, 128) || !bounded(cfg.Timezone, 128) {
		return errors.New("invalid Intelbras alarm registration input")
	}
	state.AlarmRegistered = false
	state.AlarmSessionID = ""
	if _, err := alarmPost(ctx, c.account, alarmAccountURL+"down", nil); err != nil {
		return err
	}
	hash := sha256.Sum256([]byte(cred.Password))
	login := struct {
		Account  string `xml:"account"`
		AuthCode string `xml:"auth-code"`
		Region   int    `xml:"ip-region-id"`
		Password string `xml:"password"`
		AuthType int    `xml:"auth-type"`
	}{Account: cred.Email, Password: hex.EncodeToString(hash[:])}
	clientID := "003-4077-" + clientUUID
	body, err := encodeAlarmRequest("login", "", 1, login, &alarmAccountClient{clientID, 3, "A0077,G0077", "4077"})
	if err != nil {
		return err
	}
	if _, err = alarmPost(ctx, c.account, alarmAccountURL+"up", body); err != nil {
		return err
	}
	data, err := alarmPost(ctx, c.account, alarmAccountURL+"down", nil)
	if err != nil {
		return err
	}
	account, err := decodeAlarmReply(data, "login", 1)
	if err != nil {
		return err
	}
	session := account.Header.Session.ID
	if !bounded(session, 4096) || account.Content.AccountID != cfg.UserID {
		return errors.New("Intelbras alarm account identity mismatch")
	}
	// AlarmRequestHelper.getLoginReq and AlarmLoginReqContent.Client map CID to token.
	var content alarmRegistration
	content.Client.ID = clientID
	content.Client.Token = fcmToken
	content.Client.TokenType = 1
	content.Client.Type = 3
	content.Client.OEM = "A0077,G0077"
	content.Client.App = 4077
	content.Client.Language = cfg.Language
	content.Client.Timezone = strings.Replace(cfg.Timezone, "gmt", "GMT", 1)
	content.Account.ID = cfg.UserID
	content.Account.AuthServer = alarmAccountHost
	body, err = encodeAlarmRequest("client-login", session, 0, content, nil)
	if err != nil {
		return err
	}
	data, err = alarmPost(ctx, c.alarm, alarmRegisterURL, body)
	if err != nil {
		return err
	}
	if _, err = decodeAlarmReply(data, "client-login", 0); err != nil {
		return err
	}
	state.AlarmRegistered = true
	state.AlarmSessionID = session
	return persist(state, save)
}
