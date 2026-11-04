package intelbras

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"
)

func alarmFixture(command, session, account string, seq int) string {
	return `<envelope><header><result>0</result><command>` + command + `</command><seq>` + string(rune('0'+seq)) + `</seq><session><id>` + session + `</id></session></header><content><account-id>` + account + `</account-id></content></envelope>`
}
func TestAlarmNativeOwnRegistration(t *testing.T) {
	calls, saves := 0, 0
	rawPassword := strings.Repeat("a", 64)
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		body, _ := io.ReadAll(r.Body)
		if r.Method != "POST" {
			t.Fatal("wrong method")
		}
		switch calls {
		case 1:
			if r.URL.String() != alarmAccountURL+"down" || len(body) != 0 {
				t.Fatal("wrong account prime")
			}
			resp := response("")
			resp.Header.Set("Set-Cookie", "session=synthetic; Path=/; Secure")
			return resp, nil
		case 2:
			if r.URL.String() != alarmAccountURL+"up" || !strings.Contains(r.Header.Get("Cookie"), "session=synthetic") {
				t.Fatal("missing account cookie")
			}
			var e struct {
				Header struct {
					Command string `xml:"command"`
					Seq     int    `xml:"seq"`
					Client  struct {
						ID string `xml:"id"`
					} `xml:"client"`
				} `xml:"header"`
				Content struct {
					Account  string `xml:"account"`
					Password string `xml:"password"`
				} `xml:"content"`
			}
			if xml.Unmarshal(body, &e) != nil {
				t.Fatal("bad XML")
			}
			h := sha256.Sum256([]byte(rawPassword))
			if e.Header.Command != "login" || e.Header.Seq != 1 || e.Header.Client.ID != "003-4077-"+uuid || e.Content.Password != hex.EncodeToString(h[:]) || e.Content.Account != "a@example.invalid" {
				t.Fatal("wrong login")
			}
			return response(""), nil
		case 3:
			return response(alarmFixture("login", "own-session", "123456", 1)), nil
		case 4:
			if r.URL.String() != alarmRegisterURL {
				t.Fatal("wrong alarm route")
			}
			var e struct {
				Header struct {
					Command string `xml:"command"`
					Session string `xml:"session"`
					Seq     int    `xml:"seq"`
				} `xml:"header"`
				Content alarmRegistration `xml:"content"`
			}
			if xml.Unmarshal(body, &e) != nil {
				t.Fatal("bad XML")
			}
			c := e.Content.Client
			if e.Header.Command != "client-login" || e.Header.Session != "own-session" || e.Header.Seq != 0 || c.ID != "003-4077-"+uuid || c.Token != "own-fcm" || c.TokenType != 1 || c.Type != 3 || c.OEM != "A0077,G0077" || c.App != 4077 || c.Language != "pt" || c.Timezone != "GMT-03:00" || e.Content.Account.ID != "123456" || e.Content.Account.AuthServer != alarmAccountHost {
				t.Fatal("wrong alarm registration")
			}
			if strings.Contains(string(body), "<hs>") || strings.Contains(string(body), "<lt>") {
				t.Fatal("unexpected compatibility identity")
			}
			return response(alarmFixture("client-login", "", "", 0)), nil
		default:
			t.Fatal("extra request")
			return nil, nil
		}
	})}
	state := AuthState{ClientUUID: uuid}
	if err := (&AlarmClient{account: hc, alarm: hc}).Register(context.Background(), config(), Credentials{"a@example.invalid", rawPassword}, uuid, "own-fcm", &state, func(s *AuthState) error { saves++; return nil }); err != nil {
		t.Fatal(err)
	}
	if calls != 4 || saves != 1 || !state.AlarmRegistered || state.AlarmSessionID != "own-session" {
		t.Fatal("missing saved registration")
	}
}
func TestAlarmRejectsBeforePushMutation(t *testing.T) {
	for _, tc := range []struct {
		name, body    string
		otherIdentity bool
	}{
		{"account mismatch", alarmFixture("login", "s", "654321", 1), false},
		{"missing session", alarmFixture("login", "", "123456", 1), false},
		{"command mismatch", alarmFixture("other", "s", "123456", 1), false},
		{"sequence mismatch", alarmFixture("login", "s", "123456", 2), false},
		{"malformed", "<envelope>", false},
		{"oversize", strings.Repeat("x", maxResponse+1), false},
		{"other identity", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls > 3 {
					t.Fatal("push mutation before verified account")
				}
				if calls == 3 {
					return response(tc.body), nil
				}
				return response(""), nil
			})}
			state := AuthState{ClientUUID: uuid}
			if tc.otherIdentity {
				state.ClientUUID = "bbbbbbbb-bbbb-4ccc-8ddd-eeeeeeeeeeee"
			}
			err := (&AlarmClient{account: hc, alarm: hc}).Register(context.Background(), config(), Credentials{"a@example.invalid", "raw"}, uuid, "own-token", &state, func(*AuthState) error { return nil })
			if err == nil || state.AlarmRegistered || (tc.otherIdentity && calls != 0) {
				t.Fatal("invalid registration accepted")
			}
		})
	}
}
func TestAlarmFailureAndCancellation(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		calls := 0
		ctx, cancelFunc := context.WithCancel(context.Background())
		if cancel {
			cancelFunc()
		}
		defer cancelFunc()
		hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.Context().Err() != nil {
				return nil, r.Context().Err()
			}
			if calls == 3 {
				return response(alarmFixture("login", "s", "123456", 1)), nil
			}
			if calls == 4 {
				return response(`<envelope><header><result>9</result><command>client-login</command><seq>0</seq></header></envelope>`), nil
			}
			return response(""), nil
		})}
		state := AuthState{ClientUUID: uuid, AlarmRegistered: true, AlarmSessionID: "stale"}
		err := (&AlarmClient{account: hc, alarm: hc}).Register(ctx, config(), Credentials{"a@example.invalid", "raw"}, uuid, "own-token", &state, func(*AuthState) error { return nil })
		if err == nil || state.AlarmRegistered || state.AlarmSessionID != "" {
			t.Fatal("failed native stage remained registered")
		}
		if cancel && !errors.Is(err, context.Canceled) {
			t.Fatal("lost cancellation")
		}
	}
}

func TestAlarmTLSChainPinAndRedirectRestrictions(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "synthetic CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, public, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, ca, public, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		t.Fatal(err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	client, err := NewAlarmClient(caPEM, hashBytes(string(leafDER)))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		hc   *http.Client
		host string
	}{{client.account, alarmAccountHost}, {client.alarm, alarmPushHost}} {
		tr := tc.hc.Transport.(*http.Transport)
		cfg := tr.TLSClientConfig
		if cfg.ServerName != tc.host || cfg.MinVersion < tls.VersionTLS12 || len(cfg.Certificates) != 0 || tc.hc.Timeout != 15*time.Second || tc.hc.Jar == nil || tr.Proxy != nil {
			t.Fatal("TLS restrictions missing")
		}
		cs := tls.ConnectionState{ServerName: tc.host, PeerCertificates: []*x509.Certificate{leaf}}
		if err := cfg.VerifyConnection(cs); err != nil {
			t.Fatal(err)
		}
		cs.ServerName = "other.invalid"
		if cfg.VerifyConnection(cs) == nil {
			t.Fatal("wrong SNI accepted")
		}
		cs.ServerName = tc.host
		wrongPin := *leaf
		wrongPin.Raw = []byte("other")
		roots := x509.NewCertPool()
		roots.AddCert(ca)
		if alarmVerifyPeer(roots, hashBytes(string(wrongPin.Raw)), tc.host)(cs) == nil {
			t.Fatal("wrong exact pin accepted")
		}
		if alarmVerifyPeer(x509.NewCertPool(), hashBytes(string(leaf.Raw)), tc.host)(cs) == nil {
			t.Fatal("untrusted chain accepted")
		}
		if alarmVerifyPeer(roots, hashBytes(string(leaf.Raw)), "other.invalid")(cs) == nil {
			t.Fatal("outside host accepted")
		}
		cs.PeerCertificates = nil
		if cfg.VerifyConnection(cs) == nil {
			t.Fatal("missing certificate accepted")
		}
		if tc.hc.CheckRedirect(&http.Request{}, nil) == nil {
			t.Fatal("redirect accepted")
		}
	}
	if _, err := NewAlarmClient(nil, hashBytes(string(leafDER))); err == nil {
		t.Fatal("missing CA accepted")
	}
	if _, err := NewAlarmClient(caPEM, []byte("invalid")); err == nil {
		t.Fatal("invalid pin accepted")
	}

}
func TestAlarmRejectsInvalidFinalReplyAndSaveFailure(t *testing.T) {
	for _, body := range []string{"<envelope>", strings.Repeat("x", maxResponse+1), alarmFixture("other", "", "", 0), alarmFixture("client-login", "", "", 1), `<envelope><header><command>client-login</command><seq>0</seq></header></envelope>`} {
		calls := 0
		hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			switch calls {
			case 3:
				return response(alarmFixture("login", "s", "123456", 1)), nil
			case 4:
				return response(body), nil
			default:
				return response(""), nil
			}
		})}
		state := AuthState{ClientUUID: uuid}
		saves := 0
		err := (&AlarmClient{account: hc, alarm: hc}).Register(context.Background(), config(), Credentials{"a@example.invalid", "raw"}, uuid, "own-token", &state, func(*AuthState) error { saves++; return nil })
		if err == nil || state.AlarmRegistered || saves != 0 {
			t.Fatal("invalid alarm reply registered")
		}
	}
	calls := 0
	hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 3 {
			return response(alarmFixture("login", "s", "123456", 1)), nil
		}
		if calls == 4 {
			return response(alarmFixture("client-login", "", "", 0)), nil
		}
		return response(""), nil
	})}
	state := AuthState{ClientUUID: uuid}
	err := (&AlarmClient{account: hc, alarm: hc}).Register(context.Background(), config(), Credentials{"a@example.invalid", "raw"}, uuid, "own-token", &state, func(*AuthState) error { return errors.New("private-path-secret") })
	if err == nil || strings.Contains(err.Error(), "private-path-secret") {
		t.Fatal("save failure not sanitized")
	}
}

func TestAlarmAccountMultipartAndOptionalHeaderFields(t *testing.T) {
	for _, accountBody := range []string{
		"--synthetic-boundary\r\nContent-Type: application/xml\r\n\r\n<envelope><header><result>100</result><command>login</command><seq>1</seq><session><id>own-session</id></session></header><content><account-id>123456</account-id></content></envelope>\r\n--synthetic-boundary--\r\n",
		`<envelope><header><result>0</result><session><id>own-session</id></session></header><content><account-id>123456</account-id></content></envelope>`,
	} {
		calls := 0
		hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			switch calls {
			case 3:
				return response(accountBody), nil
			case 4:
				return response(`<envelope><header><result>0</result></header></envelope>`), nil
			default:
				return response(""), nil
			}
		})}
		state := AuthState{ClientUUID: uuid}
		if err := (&AlarmClient{account: hc, alarm: hc}).Register(context.Background(), config(), Credentials{"a@example.invalid", "raw"}, uuid, "own-token", &state, func(*AuthState) error { return nil }); err != nil {
			t.Fatal(err)
		}
		if !state.AlarmRegistered || calls != 4 {
			t.Fatal("account framing blocked registration")
		}
	}
	if _, err := decodeAlarmReply([]byte(`<envelope><header><result>100</result></header></envelope>`), "client-login", 0); err == nil {
		t.Fatal("alarm result 100 accepted")
	}
}
