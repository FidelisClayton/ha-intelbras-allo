package media

import "strings"

type testCredentials struct{ body []byte }

func (testCredentials) DeviceID() string  { return "same" }
func (testCredentials) StreamKey() string { return strings.Repeat("k", 32) }
func (c testCredentials) MediaAuthBody() []byte {
	if c.body != nil {
		return c.body
	}
	return []byte("viewer&&opaque&password/+==\x00OEM123\x000123456789abcdef\x00")
}
func syntheticMediaCredentials() testCredentials { return testCredentials{} }
