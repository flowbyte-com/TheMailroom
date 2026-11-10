package smtpclient

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/flowbyte-com/themailroom/internal/config"
)

// probeConfig builds a probe configuration for the test server.
//
// The host is the name in the test certificate, not the listener address:
// the client dials the listener through the Client's DialContext seam and
// verifies the certificate against the configured host, exactly as it does
// in production.
func probeConfig(t *testing.T, server *testServer, implicitTLS bool) config.SMTPConfig {
	t.Helper()
	_, portText, err := net.SplitHostPort(server.addr())
	if err != nil {
		t.Fatalf("split address: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	cfg := config.SMTPConfig{
		Host:     "mail.example.test",
		Port:     port,
		Username: "user@example.test",
		Password: "s3cret",
	}
	if implicitTLS {
		cfg.TLS = true
	} else {
		cfg.StartTLS = true
	}
	return cfg
}

// assertNoMessageSent asserts that the probe issued no envelope command.
func assertNoMessageSent(t *testing.T, server *testServer) {
	t.Helper()
	// accepts counts accepted TCP connections, not accepted messages, so it
	// is deliberately not asserted on here. The evidence that no message was
	// sent is the absence of the envelope commands and of message data.
	commands, mailFrom, rcpts, authSeen, _, _, data, _ := server.snapshot()
	if !authSeen {
		t.Error("authentication was not attempted")
	}
	if mailFrom != "" {
		t.Errorf("MAIL FROM was issued: %q", mailFrom)
	}
	if len(rcpts) != 0 {
		t.Errorf("RCPT TO was issued: %v", rcpts)
	}
	if len(data) != 0 {
		t.Errorf("message data was issued: %d bytes", len(data))
	}
	for _, command := range commands {
		upper := strings.ToUpper(strings.TrimSpace(command))
		for _, forbidden := range []string{"MAIL FROM", "RCPT TO", "DATA"} {
			if strings.HasPrefix(upper, forbidden) {
				t.Errorf("the probe issued %s: %q", forbidden, command)
			}
		}
	}
}

// TestProbeAuthenticatesOverImplicitTLSWithoutSending covers the success
// path: the probe completes TLS and authentication and stops.
func TestProbeAuthenticatesOverImplicitTLSWithoutSending(t *testing.T) {
	cert, pool := testCertificate(t, []string{"mail.example.test"}, nil)
	server := newTestServer(t, testServerOptions{
		implicitTLS: true,
		requireAuth: true,
		username:    "user@example.test",
		password:    "s3cret",
		cert:        cert,
	})
	client := newTestClient(server, pool)

	result, err := client.Probe(context.Background(), probeConfig(t, server, true))
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !result.Authenticated {
		t.Error("Authenticated = false, want true")
	}
	if result.TLSVersion == "" {
		t.Error("TLSVersion is empty")
	}
	assertNoMessageSent(t, server)
}

// TestProbeAuthenticatesOverStartTLSWithoutSending covers the mandatory
// STARTTLS path.
func TestProbeAuthenticatesOverStartTLSWithoutSending(t *testing.T) {
	cert, pool := testCertificate(t, []string{"mail.example.test"}, nil)
	server := newTestServer(t, testServerOptions{
		startTLS:    true,
		requireAuth: true,
		username:    "user@example.test",
		password:    "s3cret",
		cert:        cert,
	})
	client := newTestClient(server, pool)

	result, err := client.Probe(context.Background(), probeConfig(t, server, false))
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !result.Authenticated {
		t.Error("Authenticated = false, want true")
	}
	assertNoMessageSent(t, server)
}

// TestProbeRejectsAuthenticationFailure covers a rejected credential. The
// credential itself must not appear in the error.
func TestProbeRejectsAuthenticationFailure(t *testing.T) {
	cert, pool := testCertificate(t, []string{"mail.example.test"}, nil)
	server := newTestServer(t, testServerOptions{
		implicitTLS: true,
		requireAuth: true,
		username:    "user@example.test",
		password:    "s3cret",
		cert:        cert,
	})
	client := newTestClient(server, pool)

	cfg := probeConfig(t, server, true)
	cfg.Password = "wrong-password-value"
	_, err := client.Probe(context.Background(), cfg)
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("err = %v, want ErrAuthFailed", err)
	}
	if strings.Contains(err.Error(), "wrong-password-value") {
		t.Errorf("the error leaks the credential: %v", err)
	}
	assertNoMessageSent(t, server)
}

// TestProbeRejectsACertificateForAnotherHost covers hostname verification.
func TestProbeRejectsACertificateForAnotherHost(t *testing.T) {
	cert, pool := testCertificate(t, []string{"mail.example.test"}, nil)
	server := newTestServer(t, testServerOptions{
		implicitTLS: true,
		cert:        cert,
	})
	client := newTestClient(server, pool)

	cfg := probeConfig(t, server, true)
	cfg.Host = "other.example.test"
	_, err := client.Probe(context.Background(), cfg)
	if !errors.Is(err, ErrTLSFailed) {
		t.Fatalf("err = %v, want ErrTLSFailed", err)
	}
}

// TestProbeRejectsAnUntrustedCertificate covers certificate verification
// against a pool that does not contain the server's authority.
func TestProbeRejectsAnUntrustedCertificate(t *testing.T) {
	cert, _ := testCertificate(t, []string{"mail.example.test"}, nil)
	_, otherPool := testCertificate(t, []string{"mail.example.test"}, nil)
	server := newTestServer(t, testServerOptions{
		implicitTLS: true,
		cert:        cert,
	})
	client := newTestClient(server, otherPool)

	_, err := client.Probe(context.Background(), probeConfig(t, server, true))
	if !errors.Is(err, ErrTLSFailed) {
		t.Fatalf("err = %v, want ErrTLSFailed", err)
	}
}

// TestProbeHonoursCancellation covers a server that stalls during the
// handshake: the probe must return within the bound instead of hanging.
func TestProbeHonoursCancellation(t *testing.T) {
	cert, pool := testCertificate(t, []string{"mail.example.test"}, nil)
	server := newTestServer(t, testServerOptions{
		implicitTLS: true,
		cert:        cert,
		noEHLO:      true,
	})
	client := newTestClient(server, pool)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := client.Probe(ctx, probeConfig(t, server, true))
	elapsed := time.Since(start)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("Probe took %s; the context bound was not honoured", elapsed)
	}
}

// TestProbeRejectsAnAlreadyCancelledContext covers the pre-flight check.
func TestProbeRejectsAnAlreadyCancelledContext(t *testing.T) {
	client := New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := client.Probe(ctx, config.SMTPConfig{
		Host: "mail.example.test", Port: 587, StartTLS: true,
		Username: "user@example.test", Password: "s3cret",
	})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
}

// TestProbeRejectsInvalidConfiguration covers the configuration checks that
// Send also performs.
func TestProbeRejectsInvalidConfiguration(t *testing.T) {
	cases := map[string]config.SMTPConfig{
		"no TLS mode": {
			Host: "mail.example.test", Port: 25,
			Username: "user@example.test", Password: "s3cret",
		},
		"both TLS modes": {
			Host: "mail.example.test", Port: 587, TLS: true, StartTLS: true,
			Username: "user@example.test", Password: "s3cret",
		},
		"no credential": {
			Host: "mail.example.test", Port: 587, StartTLS: true,
			Username: "user@example.test",
		},
		"both credential sources": {
			Host: "mail.example.test", Port: 587, StartTLS: true,
			Username: "user@example.test", Password: "s3cret",
			PasswordCommand: []string{"printf", "s3cret"},
		},
		"invalid port": {
			Host: "mail.example.test", Port: 0, StartTLS: true,
			Username: "user@example.test", Password: "s3cret",
		},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := New().Probe(context.Background(), cfg); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("err = %v, want ErrInvalidRequest", err)
			}
		})
	}
}
