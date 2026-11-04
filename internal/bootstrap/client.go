package bootstrap

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strconv"
	"strings"
	"sync"
	"time"
)

const cloudHost = "intelbras-4.qvcloud.net"
const maxBody = 2 << 20

type Device struct {
	ID       string `xml:"id" json:"id"`
	Name     string `xml:"name" json:"name"`
	Model    string `xml:"model" json:"model"`
	Channels *int   `xml:"channel-num" json:"channel_count"`
}
type resultError struct{ Command, Code string }

func (e *resultError) Error() string { return e.Command + " failed (result=" + e.Code + ")" }

type envelope struct {
	XMLName xml.Name `xml:"envelope"`
	Header  struct {
		Flag     string `xml:"flag"`
		Version  string `xml:"version"`
		Command  string `xml:"command"`
		Seq      int    `xml:"seq"`
		Session  string `xml:"session"`
		UserData string `xml:"user-data"`
		Client   struct {
			ID   string `xml:"id"`
			Type int    `xml:"type"`
			OEM  string `xml:"oem"`
			App  string `xml:"app"`
		} `xml:"client"`
	} `xml:"header"`
	Content any `xml:"content"`
}
type loginContent struct {
	Account  string `xml:"account"`
	AuthCode string `xml:"auth-code"`
	Region   int    `xml:"ip-region-id"`
	Password string `xml:"password"`
	AuthType int    `xml:"auth-type"`
}
type listContent struct {
	Filter string `xml:"filter"`
	Order  int    `xml:"order"`
	Count  int    `xml:"count"`
	Page   int    `xml:"page"`
	Owner  string `xml:"owner"`
}

func requestXML(command string, seq int, id, session string, content any) ([]byte, error) {
	var e envelope
	e.Header.Flag = "tdkcloud"
	e.Header.Version = "v1.13"
	e.Header.Command = command
	e.Header.Seq = seq
	e.Header.Session = session
	e.Header.Client.ID = id
	e.Header.Client.Type = 3
	e.Header.Client.OEM = "A0077,G0077"
	e.Header.Client.App = "4077"
	e.Content = content
	b, err := xml.Marshal(e)
	return append([]byte(xml.Header), b...), err
}

type response struct {
	Header struct {
		Result  string `xml:"result"`
		Command string `xml:"command"`
		Seq     *int   `xml:"seq"`
		Session struct {
			ID string `xml:"id"`
		} `xml:"session"`
	} `xml:"header"`
	Result           string
	Devices          []Device
	TokenID          string
	DynamicPassword  string
	PasswordExpired  string
	StreamKey        string
	TokenOutAuthCode string
	AccountID        string
}

func parseResponse(raw []byte, command string) (response, error) {
	var out response
	start := bytes.Index(raw, []byte("<envelope"))
	if start < 0 {
		return out, errors.New("response has no XML envelope")
	}
	dec := xml.NewDecoder(bytes.NewReader(raw[start:]))
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return out, errors.New("invalid response XML")
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth == 1 && t.Name.Local != "envelope" {
				return out, errors.New("invalid response envelope")
			}
			switch t.Name.Local {
			case "header":
				if err := dec.DecodeElement(&out.Header, &t); err != nil {
					return out, errors.New("invalid response header")
				}
				if out.Header.Result != "" {
					out.Result = out.Header.Result
				}
				depth--
			case "result":
				if err := dec.DecodeElement(&out.Result, &t); err != nil {
					return out, errors.New("invalid response result")
				}
				depth--
			case "account-id", "device-id", "dynamic-password", "password-expired", "out-auth-code", "data-encode-key":
				var value string
				if err := dec.DecodeElement(&value, &t); err != nil {
					return out, errors.New("invalid response token")
				}
				switch t.Name.Local {
				case "account-id":
					out.AccountID = value
				case "device-id":
					out.TokenID = value
				case "dynamic-password":
					out.DynamicPassword = value
				case "password-expired":
					out.PasswordExpired = value
				case "out-auth-code":
					out.TokenOutAuthCode = value
				case "data-encode-key":
					out.StreamKey = value
				}
				depth--
			case "device":
				var d Device
				if err := dec.DecodeElement(&d, &t); err != nil {
					return out, errors.New("invalid response device")
				}
				out.Devices = append(out.Devices, d)
				depth--
			}
		case xml.EndElement:
			depth--
			if depth == 0 {
				if out.Result != "0" && out.Result != "100" {
					if _, err := strconv.ParseUint(out.Result, 10, 32); err != nil {
						return response{}, errors.New("missing or invalid result code")
					}
					return response{}, &resultError{command, out.Result}
				}
				return out, nil
			}
		}
	}
}
func post(ctx context.Context, hc *http.Client, direction string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+cloudHost+":443/auth/user;jus_duplex="+direction, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("cannot build request")
	}
	req.Header.Set("Content-Type", "application/xml;charset=utf-8")
	req.Header.Set("User-Agent", "okhttp/3.12.13")
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("cloud request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("cloud HTTP status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, errors.New("cannot read cloud response")
	}
	if len(b) > maxBody {
		return nil, errors.New("cloud response exceeds limit")
	}
	return b, nil
}

//go:embed assets/qualvision-ca.pem
var approvedCAPEM []byte

// VendorCA returns the approved public trust anchor for the vendor alarm client.
func VendorCA() []byte { return bytes.Clone(approvedCAPEM) }

// Client authenticates only against the approved Intelbras account endpoint.
// It never loads local credentials, native profiles or TLS client keys.
type Client struct{ http *http.Client }

func NewClient() (*Client, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(approvedCAPEM) {
		return nil, errors.New("invalid embedded vendor CA")
	}
	raw, err := hex.DecodeString("e7167be07fc554614ce611b1be054c29cfc39926695d6a6717c9e631dc83276a")
	if err != nil {
		return nil, err
	}
	return newClient(roots, [32]byte(raw)), nil
}

func newClient(roots *x509.CertPool, pin [32]byte) *Client {
	// The vendor leaf has no DNS names; verified chain, fixed host and exact pin replace hostname verification.
	config := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: cloudHost, InsecureSkipVerify: true, VerifyConnection: verifyPeer(roots, pin)}
	transport := &http.Transport{TLSClientConfig: config, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 15 * time.Second}
	jar, _ := cookiejar.New(nil)
	return &Client{http: &http.Client{Transport: transport, Jar: jar, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirects prohibited") }}}
}

func verifyPeer(roots *x509.CertPool, pin [32]byte) func(tls.ConnectionState) error {
	return func(cs tls.ConnectionState) error {
		if cs.ServerName != cloudHost {
			return errors.New("unapproved TLS host")
		}
		if len(cs.PeerCertificates) == 0 {
			return errors.New("missing TLS peer certificate")
		}
		leaf := cs.PeerCertificates[0]
		inter := x509.NewCertPool()
		for _, cert := range cs.PeerCertificates[1:] {
			inter.AddCert(cert)
		}
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
			return errors.New("TLS peer chain verification failed")
		}
		if sha256.Sum256(leaf.Raw) != pin {
			return errors.New("TLS peer pin mismatch")
		}
		return nil
	}
}

func (c *Client) Close() { c.http.CloseIdleConnections() }

// Session contains account state in memory. Requests are serialized for the duplex protocol.
type Session struct {
	http                           *http.Client
	clientID, sessionID, accountID string
	seq                            int
	mu                             sync.Mutex
}

func (c *Client) Login(ctx context.Context, email, password string) (*Session, error) {
	if email == "" || password == "" {
		return nil, errors.New("email and raw password required")
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return nil, errors.New("cannot generate client ID")
	}
	s := &Session{http: c.http, clientID: "003-4077-" + hex.EncodeToString(id)}
	if _, err := post(ctx, c.http, "down", nil); err != nil {
		return nil, err
	}
	hash := sha256.Sum256([]byte(password))
	out, err := s.command(ctx, "login", loginContent{Account: email, Password: hex.EncodeToString(hash[:])})
	if err != nil {
		return nil, err
	}
	if out.Header.Session.ID == "" {
		return nil, errors.New("login response has no session ID")
	}
	s.sessionID = out.Header.Session.ID
	s.accountID = out.AccountID
	return s, nil
}

func (s *Session) command(ctx context.Context, command string, content any) (response, error) {
	s.seq++
	b, err := requestXML(command, s.seq, s.clientID, s.sessionID, content)
	if err != nil {
		return response{}, errors.New("cannot encode cloud request")
	}
	if _, err := post(ctx, s.http, "up", b); err != nil {
		return response{}, err
	}
	raw, err := post(ctx, s.http, "down", nil)
	if err != nil {
		return response{}, err
	}
	out, err := parseResponse(raw, command)
	if err != nil {
		return response{}, err
	}
	if out.Header.Command != "" && out.Header.Command != command {
		return response{}, errors.New("cloud response command mismatch")
	}
	if out.Header.Seq != nil && *out.Header.Seq != s.seq {
		return response{}, errors.New("cloud response sequence mismatch")
	}
	return out, nil
}

func (s *Session) list(ctx context.Context) ([]Device, error) {
	out, err := s.command(ctx, "get-device-list", listContent{})
	if err != nil {
		return nil, err
	}
	if out.Devices == nil {
		out.Devices = []Device{}
	}
	return out.Devices, nil
}

func (s *Session) ListDevices(ctx context.Context) ([]Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.list(ctx)
}

// DeviceCredentials holds fresh opaque media credentials only in memory.
// It deliberately does not provide JSON or text serialization of secrets.
type DeviceCredentials struct{ deviceID, dynamicPassword, streamKey string }

func (c DeviceCredentials) DeviceID() string  { return c.deviceID }
func (c DeviceCredentials) StreamKey() string { return c.streamKey }
func (c DeviceCredentials) MediaAuthBody() []byte {
	return []byte("adminapp&&" + c.dynamicPassword + "\x00\x00\x00")
}
func (c DeviceCredentials) String() string   { return "[device credentials redacted]" }
func (c DeviceCredentials) GoString() string { return c.String() }

func (s *Session) PrepareDeviceCredentials(ctx context.Context, id string) (DeviceCredentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" {
		return DeviceCredentials{}, errors.New("selected device ID required")
	}
	devices, err := s.list(ctx)
	if err != nil {
		return DeviceCredentials{}, err
	}
	matches := 0
	for _, d := range devices {
		if d.ID == id {
			matches++
		}
	}
	if matches != 1 {
		return DeviceCredentials{}, errors.New("exactly one account device must match selection")
	}
	token, err := s.command(ctx, "get-device-token", struct {
		ID string `xml:"device-id"`
	}{id})
	if err != nil {
		return DeviceCredentials{}, err
	}
	if token.TokenID != id {
		return DeviceCredentials{}, errors.New("token device identity mismatch")
	}
	if len(token.DynamicPassword) == 0 || len(token.DynamicPassword) > 4096 || strings.Contains(token.DynamicPassword, "&&") || len(token.StreamKey) != 32 {
		return DeviceCredentials{}, errors.New("invalid native media credentials")
	}
	for _, c := range []byte(token.DynamicPassword) {
		if c < 0x20 || c > 0x7e {
			return DeviceCredentials{}, errors.New("invalid native media credentials")
		}
	}
	return DeviceCredentials{deviceID: id, dynamicPassword: token.DynamicPassword, streamKey: token.StreamKey}, nil
}
