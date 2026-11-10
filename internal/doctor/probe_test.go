package doctor

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"testing"
	"time"
)

// selfSignedListener starts a TLS listener with a certificate that no root
// store trusts, and returns its address.
func selfSignedListener(t *testing.T, dnsNames []string) string {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{Organization: []string{"Doctor Test"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     dnsNames,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(2 * time.Second))
				// Complete the handshake so the client sees the
				// certificate and rejects it on trust grounds.
				server := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}})
				_ = server.Handshake()
			}(conn)
		}
	}()
	return ln.Addr().String()
}

// TestRealProbeRejectsAnUntrustedCertificate proves that the production
// probe verifies certificates: a self-signed certificate is refused because
// it is not signed by a trusted authority.
func TestRealProbeRejectsAnUntrustedCertificate(t *testing.T) {
	addr := selfSignedListener(t, []string{"mail.example.test"})
	probe := realProbe{dialTimeout: 2 * time.Second, handshakeTimeout: 2 * time.Second}

	_, err := probe.TLSState(context.Background(), addr, "mail.example.test")
	if err == nil {
		t.Fatal("TLSState accepted a self-signed certificate")
	}
	var authority x509.UnknownAuthorityError
	if !errors.As(err, &authority) {
		t.Fatalf("err = %v, want an unknown-authority failure", err)
	}
}

// TestRealProbeRejectsAnUnreachableEndpoint covers a connection refusal.
func TestRealProbeRejectsAnUnreachableEndpoint(t *testing.T) {
	// Bind then close, so the port is almost certainly free.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	probe := realProbe{dialTimeout: time.Second, handshakeTimeout: time.Second}
	_, err = probe.TLSState(context.Background(), addr, "mail.example.test")
	if err == nil {
		t.Fatal("TLSState succeeded against a closed port")
	}
	if got := classifyErr(err); got != "connection refused" && got != "failed" {
		t.Errorf("classifyErr(%v) = %q, want a connection failure", err, got)
	}
}

// TestClassifyErrIsSanitised covers the classifications an operator sees.
func TestClassifyErrIsSanitised(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"deadline", context.DeadlineExceeded, "timed out"},
		{"cancelled", context.Canceled, "cancelled"},
		{"dns not found", &net.DNSError{Err: "no such host", IsNotFound: true}, "host not found"},
		{"expired", x509.CertificateInvalidError{Reason: x509.Expired}, "certificate rejected: certificate has expired or is not yet valid"},
		{"hostname", x509.HostnameError{Host: "mail.example.test"}, "certificate does not match the host name"},
		{"authority", x509.UnknownAuthorityError{}, "certificate is not signed by a trusted authority"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyErr(tc.err); got != tc.want {
				t.Errorf("classifyErr(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}
