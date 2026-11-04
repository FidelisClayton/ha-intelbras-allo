package bootstrap

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/xml"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER, caDER}, PrivateKey: leafKey}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(testCA(t, server))
	c := newClient(roots, sha256.Sum256(server.Certificate().Raw))
	transport := c.http.Transport.(*http.Transport)
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != cloudHost+":443" {
			t.Errorf("unapproved destination %s", addr)
		}
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	t.Cleanup(c.Close)
	return c, server
}

func testCA(t *testing.T, server *httptest.Server) *x509.Certificate {
	t.Helper()
	cert, err := x509.ParseCertificate(server.TLS.Certificates[0].Certificate[1])
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func TestAccountWire(t *testing.T) {
	for _, result := range []string{"0", "100"} {
		t.Run(result, func(t *testing.T) {
			var pending, password string
			calls := 0
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if len(r.TLS.PeerCertificates) != 0 {
					t.Error("client certificate sent")
				}
				if r.URL.Path != "/auth/user;jus_duplex=up" && r.URL.Path != "/auth/user;jus_duplex=down" {
					t.Errorf("route %s", r.URL.Path)
				}
				if strings.HasSuffix(r.URL.Path, "=up") {
					var req struct {
						Header struct {
							Command string `xml:"command"`
							Session string `xml:"session"`
						} `xml:"header"`
						Content struct {
							Password string `xml:"password"`
							ID       string `xml:"device-id"`
						} `xml:"content"`
					}
					b, _ := io.ReadAll(r.Body)
					if err := xml.Unmarshal(b, &req); err != nil {
						t.Error(err)
					}
					pending = req.Header.Command
					if pending == "login" {
						password = req.Content.Password
					} else if req.Header.Session != "synthetic-session" {
						t.Error("missing session")
					}
					if pending == "get-device-token" && req.Content.ID != "synthetic-device" {
						t.Error("wrong selected ID")
					}
					return
				}
				if pending == "" {
					return
				}
				content := ""
				switch pending {
				case "login":
					content = "<account-id>synthetic-account</account-id>"
				case "get-device-list":
					content = "<device><id>synthetic-device</id><name>Door</name><out-auth-code>synthetic-auth</out-auth-code></device>"
				case "get-device-token":
					content = "<device-id>synthetic-device</device-id><dynamic-password>dynamic</dynamic-password><data-encode-key>01234567890123456789012345678901</data-encode-key>"
				}
				io.WriteString(w, "multipart-prefix\r\n<envelope><header><result>"+result+"</result><command>"+pending+"</command><session><id>synthetic-session</id></session></header><content>"+content+"</content></envelope>\r\n--boundary--")
			})
			raw := strings.Repeat("a", 64)
			session, err := c.Login(context.Background(), "synthetic@example.invalid", raw)
			if err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256([]byte(raw))
			if password != hex.EncodeToString(hash[:]) {
				t.Fatal("raw password was not hashed")
			}
			devices, err := session.ListDevices(context.Background())
			if err != nil || len(devices) != 1 {
				t.Fatalf("devices: %v %v", devices, err)
			}
			credentials, err := session.PrepareDeviceCredentials(context.Background(), devices[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			if string(credentials.MediaAuthBody()) != "adminapp&&dynamic\x00\x00\x00" {
				t.Fatal("wrong media auth body")
			}
			if calls != 9 {
				t.Fatalf("unexpected requests/retry: %d", calls)
			}
		})
	}
}

func TestTrustAndBoundaries(t *testing.T) {
	for _, scenario := range []string{"pin", "chain", "redirect", "oversize", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			hits := 0
			c, server := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				hits++
				switch scenario {
				case "redirect":
					http.Redirect(w, r, "https://example.invalid/", 302)
				case "oversize":
					io.WriteString(w, strings.Repeat("x", maxBody+1))
				}
			})
			cfg := c.http.Transport.(*http.Transport).TLSClientConfig
			if scenario == "pin" {
				roots := x509.NewCertPool()
				// Preserve the real test server chain while supplying the wrong pin.
				roots.AddCert(testCA(t, server))
				cfg.VerifyConnection = verifyPeer(roots, [32]byte{})
			}
			if scenario == "chain" {
				cfg.VerifyConnection = verifyPeer(x509.NewCertPool(), sha256.Sum256(server.Certificate().Raw))
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if scenario == "cancel" {
				cancel()
			}
			_, err := c.Login(ctx, "synthetic@example.invalid", "raw")
			if err == nil {
				t.Fatal("unsafe request accepted")
			}
			if hits > 1 {
				t.Fatalf("retried/followed: %d", hits)
			}
		})
	}
}

func TestVerifierRequiresChainPinAndHost(t *testing.T) {
	c, s := testClient(t, func(http.ResponseWriter, *http.Request) {})
	_ = c
	roots := x509.NewCertPool()
	roots.AddCert(testCA(t, s))
	pin := sha256.Sum256(s.Certificate().Raw)
	state := tls.ConnectionState{ServerName: cloudHost, PeerCertificates: []*x509.Certificate{s.Certificate()}}
	if err := verifyPeer(roots, pin)(state); err != nil {
		t.Fatal(err)
	}
	if err := verifyPeer(roots, [32]byte{})(state); err == nil {
		t.Fatal("wrong pin accepted")
	}
	if err := verifyPeer(x509.NewCertPool(), pin)(state); err == nil {
		t.Fatal("wrong chain accepted")
	}
	state.ServerName = "example.invalid"
	if err := verifyPeer(roots, pin)(state); err == nil {
		t.Fatal("wrong host accepted")
	}
}

func TestNewClientWithoutHomeState(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c, err := NewClient()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cfg := c.http.Transport.(*http.Transport).TLSClientConfig
	if len(cfg.Certificates) != 0 || cfg.GetClientCertificate != nil {
		t.Fatal("client certificate configured")
	}
}

func TestTokenIdentityMismatch(t *testing.T) {
	for _, id := range []string{"wrong", ""} {
		t.Run(id, func(t *testing.T) {
			pending := ""
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "=up") {
					b, _ := io.ReadAll(r.Body)
					if strings.Contains(string(b), "get-device-list") {
						pending = "list"
					} else {
						pending = "token"
					}
					return
				}
				content := "<device><id>selected</id><out-auth-code>auth</out-auth-code></device>"
				if pending == "token" {
					content = "<device-id>" + id + "</device-id><dynamic-password>dynamic</dynamic-password><data-encode-key>01234567890123456789012345678901</data-encode-key>"
				}
				io.WriteString(w, "<envelope><header><result>0</result></header><content>"+content+"</content></envelope>")
			})
			session := &Session{http: c.http}
			if _, err := session.PrepareDeviceCredentials(context.Background(), "selected"); err == nil {
				t.Fatal("mismatched/missing identity accepted")
			}
		})
	}
}

func TestRejectInvalidCloudResponses(t *testing.T) {
	for _, body := range []string{
		`<envelope><header><result>401</result></header></envelope>`,
		`<envelope><header><result>0</result><command>wrong</command></header></envelope>`,
		`<envelope><header><result>0</result><seq>999</seq></header></envelope>`,
		`<envelope><header><result>0</result></header></envelope>`,
		`<envelope><header>`,
	} {
		t.Run(body, func(t *testing.T) {
			calls := 0
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) { calls++; io.WriteString(w, body) })
			if _, err := c.Login(context.Background(), "synthetic@example.invalid", "secret"); err == nil {
				t.Fatal("invalid login response accepted")
			}
			if calls != 3 {
				t.Fatalf("unexpected retry count %d", calls)
			}
		})
	}
}

func TestSelectedDeviceMustBeInAccountInventory(t *testing.T) {
	calls := 0
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		io.WriteString(w, `<envelope><header><result>0</result></header><content><device><id>different-device</id></device></content></envelope>`)
	})
	session := &Session{http: c.http}
	if _, err := session.PrepareDeviceCredentials(context.Background(), "selected"); err == nil {
		t.Fatal("device outside account inventory accepted")
	}
	if calls != 2 {
		t.Fatalf("token request issued for unlisted device: %d calls", calls)
	}
}
