package transport_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/punt-labs/mcp-proxy/internal/debuglog"
	"github.com/punt-labs/mcp-proxy/internal/transport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// selfSignedCA generates a self-signed CA certificate and returns the
// certificate, its PEM encoding, and its private key.
func selfSignedCA(t *testing.T) (*x509.Certificate, []byte, *ecdsa.PrivateKey) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)

	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)

	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return cert, pemBytes, key
}

// tlsServerWithCA starts a TLS httptest.Server using a leaf cert signed by the
// given CA. The server upgrades connections to WebSocket at "/" and echoes messages.
func tlsServerWithCA(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey) *httptest.Server {
	t.Helper()

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}

	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca, &leafKey.PublicKey, caKey)
	require.NoError(t, err)

	tlsCert := tls.Certificate{
		Certificate: [][]byte{leafDER},
		PrivateKey:  leafKey,
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		// Echo one message then close.
		_, msg, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		_ = conn.Write(r.Context(), websocket.MessageText, msg)
	})

	srv := httptest.NewUnstartedServer(handler)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{tlsCert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// wsURL converts an https:// URL to wss://.
func wsURL(httpURL string) string {
	return "wss" + httpURL[len("https"):]
}

func TestDial_EmptyCACert_NoCustomTLS(t *testing.T) {
	// When caCertPath is empty, Dial uses system roots (no custom TLS config).
	// Connect to a plain ws:// server — this verifies no regression on the happy path.
	d := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		conn.Close(websocket.StatusNormalClosure, "")
	}))
	t.Cleanup(d.Close)

	rawURL := "ws" + d.URL[len("http"):] + "/"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logger := debuglog.NewTestLogger(t).Logger
	conn, err := transport.Dial(ctx, rawURL, 1, nil, "", logger)
	require.NoError(t, err)
	conn.Close(websocket.StatusNormalClosure, "")
}

func TestDial_ValidCACert_CustomPoolUsed(t *testing.T) {
	// When caCertPath points to a valid CA cert, Dial uses the custom pool and
	// can connect to a server whose leaf cert is signed by that CA.
	ca, caPEM, caKey := selfSignedCA(t)
	srv := tlsServerWithCA(t, ca, caKey)

	// Write CA cert to a temp file.
	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.crt")
	require.NoError(t, os.WriteFile(caFile, caPEM, 0o600))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logger := debuglog.NewTestLogger(t).Logger
	conn, err := transport.Dial(ctx, wsURL(srv.URL)+"/", 1, nil, caFile, logger)
	require.NoError(t, err)

	// Send a message to confirm the connection is live.
	writeCtx, writeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer writeCancel()
	err = conn.Write(writeCtx, websocket.MessageText, []byte("ping"))
	require.NoError(t, err)
	_, msg, err := conn.Read(writeCtx)
	require.NoError(t, err)
	assert.Equal(t, []byte("ping"), msg)

	conn.Close(websocket.StatusNormalClosure, "")
}

func TestDial_MissingCACertFile_Error(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logger := debuglog.NewTestLogger(t).Logger
	// Use a real wss URL; the error should occur before the dial attempt
	// when the cert file cannot be read.
	_, err := transport.Dial(ctx, "wss://127.0.0.1:9999/mcp", 1, nil, "/no/such/file.crt", logger)
	require.Error(t, err)

	var certErr *transport.CACertError
	require.ErrorAs(t, err, &certErr)
	assert.Contains(t, certErr.Error(), "loading CA cert")
	assert.Contains(t, certErr.Error(), "/no/such/file.crt")
}

func TestDial_InvalidCACertPEM_Error(t *testing.T) {
	// A file that exists but contains no valid PEM cert blocks.
	dir := t.TempDir()
	badFile := filepath.Join(dir, "bad.crt")
	require.NoError(t, os.WriteFile(badFile, []byte("not a cert"), 0o600))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logger := debuglog.NewTestLogger(t).Logger
	_, err := transport.Dial(ctx, "wss://127.0.0.1:9999/mcp", 1, nil, badFile, logger)
	require.Error(t, err)

	var certErr *transport.CACertError
	require.ErrorAs(t, err, &certErr)
	assert.Contains(t, certErr.Error(), "no valid certificate blocks found")
}

func TestDial_WrongCACert_Rejected(t *testing.T) {
	// A server cert signed by CA1 must be rejected when the client trusts only CA2.
	ca1, ca1Cert, ca1Key := selfSignedCA(t)
	_, ca2PEM, _ := selfSignedCA(t)

	srv := tlsServerWithCA(t, ca1, ca1Key)

	// Write CA2's PEM — the wrong CA — as the trusted cert.
	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca2.crt")
	require.NoError(t, os.WriteFile(caFile, ca2PEM, 0o600))

	_ = ca1Cert // used only to sign the server cert

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logger := debuglog.NewTestLogger(t).Logger
	_, err := transport.Dial(ctx, wsURL(srv.URL)+"/", 1, nil, caFile, logger)
	require.Error(t, err)

	// The error must not be a CACertError — the cert file loaded fine.
	// The failure must be TLS verification.
	var certErr *transport.CACertError
	assert.False(t, errors.As(err, &certErr), "expected TLS verification error, not CACertError")
}

func TestDialHook_WrongCACert_Rejected(t *testing.T) {
	// Same property as TestDial_WrongCACert_Rejected, but through DialHook.
	ca1, ca1Cert, ca1Key := selfSignedCA(t)
	_, ca2PEM, _ := selfSignedCA(t)

	srv := tlsServerWithCA(t, ca1, ca1Key)

	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca2.crt")
	require.NoError(t, os.WriteFile(caFile, ca2PEM, 0o600))

	_ = ca1Cert // used only to sign the server cert

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logger := debuglog.NewTestLogger(t).Logger
	_, err := transport.DialHook(ctx, wsURL(srv.URL)+"/", 1, nil, caFile, logger)
	require.Error(t, err)

	var certErr *transport.CACertError
	assert.False(t, errors.As(err, &certErr), "expected TLS verification error, not CACertError")
}
