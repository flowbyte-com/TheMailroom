package smtpclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flowbyte-com/themailroom/internal/config"
)

func TestTLSConfigRequiresTLS12AndHostnameVerification(t *testing.T) {
	cfg := (&Client{}).tlsConfig("mail.example.test")
	if cfg.ServerName != "mail.example.test" {
		t.Errorf("ServerName = %q", cfg.ServerName)
	}
	if cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify is enabled")
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %x, want TLS 1.2", cfg.MinVersion)
	}
}

func TestImplicitTLSAuthenticatedSubmission(t *testing.T) {
	cert, pool := testCertificate(t, nil, []net.IP{net.ParseIP("127.0.0.1")})
	server := newTestServer(t, testServerOptions{
		implicitTLS: true,
		requireAuth: true,
		username:    "user@example.test",
		password:    "s3cret",
		cert:        cert,
	})
	req := testRequest(server, true)
	sender := newTestClient(server, pool)

	result, err := sender.Send(context.Background(), req)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if result.MessageID != req.MessageID {
		t.Errorf("MessageID = %q, want %q", result.MessageID, req.MessageID)
	}
	commands, mailFrom, rcpts, authSeen, authBeforeTLS, authPayload, data, accepts := server.snapshot()
	if accepts != 1 {
		t.Errorf("accepts = %d, want 1", accepts)
	}
	if !authSeen || authBeforeTLS {
		t.Errorf("authSeen=%v authBeforeTLS=%v, want authenticated TLS connection", authSeen, authBeforeTLS)
	}
	if !bytes.Contains(authPayload, []byte("\x00user@example.test\x00s3cret")) {
		t.Errorf("PLAIN payload did not contain expected fields")
	}
	if mailFrom != "Sales@example.test" {
		t.Errorf("MAIL FROM = %q, want local-part case preserved and domain lowercased", mailFrom)
	}
	wantRcpts := []string{"alice@example.test", "Bob@example.test"}
	if len(rcpts) != len(wantRcpts) {
		t.Fatalf("rcpts = %v, want %v", rcpts, wantRcpts)
	}
	for i := range wantRcpts {
		if rcpts[i] != wantRcpts[i] {
			t.Errorf("rcpts[%d] = %q, want %q", i, rcpts[i], wantRcpts[i])
		}
	}
	wantData := bytes.ReplaceAll(req.Message, []byte("\r\n"), []byte("\n"))
	if !bytes.Equal(data, wantData) {
		t.Errorf("transmitted data differs from prepared message: got %d bytes %q, want %d bytes %q", len(data), data, len(wantData), wantData)
	}
	if !hasPrefix(commands, "DATA") {
		t.Errorf("DATA was not issued")
	}
}

func TestStartTLSAuthenticatedSubmission(t *testing.T) {
	cert, pool := testCertificate(t, []string{"mail.example.test"}, nil)
	server := newTestServer(t, testServerOptions{
		startTLS:    true,
		requireAuth: true,
		username:    "user@example.test",
		password:    "s3cret",
		cert:        cert,
	})
	req := testRequest(server, false)
	req.Config.Host = "mail.example.test"
	sender := newTestClient(server, pool)

	if _, err := sender.Send(context.Background(), req); err != nil {
		t.Fatalf("Send: %v", err)
	}
	_, _, _, authSeen, authBeforeTLS, _, _, accepts := server.snapshot()
	if accepts != 1 || !authSeen || authBeforeTLS {
		t.Errorf("accepts=%d authSeen=%v authBeforeTLS=%v", accepts, authSeen, authBeforeTLS)
	}
	if !server.hasCommand("STARTTLS") {
		t.Error("STARTTLS was not issued")
	}
}

func TestAuthRequiredNotAdvertised(t *testing.T) {
	cert, pool := testCertificate(t, nil, []net.IP{net.ParseIP("127.0.0.1")})
	server := newTestServer(t, testServerOptions{
		implicitTLS: true,
		requireAuth: false,
		cert:        cert,
	})
	sender := newTestClient(server, pool)
	_, err := sender.Send(context.Background(), testRequest(server, true))
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("err = %v, want ErrAuthFailed", err)
	}
	if server.hasCommand("AUTH") || server.hasCommand("MAIL FROM:") {
		t.Error("AUTH or MAIL FROM was sent without an advertised mechanism")
	}
}

func TestAuthRejected(t *testing.T) {
	cert, pool := testCertificate(t, nil, []net.IP{net.ParseIP("127.0.0.1")})
	server := newTestServer(t, testServerOptions{
		implicitTLS: true,
		requireAuth: true,
		username:    "user@example.test",
		password:    "different",
		cert:        cert,
	})
	sender := newTestClient(server, pool)
	_, err := sender.Send(context.Background(), testRequest(server, true))
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("err = %v, want ErrAuthFailed", err)
	}
	if strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "different") {
		t.Fatalf("error leaked credentials: %v", err)
	}
	if server.hasCommand("MAIL FROM:") {
		t.Error("MAIL FROM followed failed authentication")
	}
}

func TestMissingSTARTTLS(t *testing.T) {
	cert, pool := testCertificate(t, nil, []net.IP{net.ParseIP("127.0.0.1")})
	server := newTestServer(t, testServerOptions{
		startTLS: false,
		cert:     cert,
	})
	sender := newTestClient(server, pool)
	_, err := sender.Send(context.Background(), testRequest(server, false))
	if !errors.Is(err, ErrTLSFailed) {
		t.Fatalf("err = %v, want ErrTLSFailed", err)
	}
	if server.hasCommand("AUTH") {
		t.Error("AUTH was issued without TLS")
	}
}

func TestSTARTTLSRefused(t *testing.T) {
	cert, pool := testCertificate(t, nil, []net.IP{net.ParseIP("127.0.0.1")})
	server := newTestServer(t, testServerOptions{
		startTLS:     true,
		startTLSCode: 454,
		requireAuth:  true,
		username:     "user@example.test",
		password:     "s3cret",
		cert:         cert,
	})
	sender := newTestClient(server, pool)
	_, err := sender.Send(context.Background(), testRequest(server, false))
	if !errors.Is(err, ErrTLSFailed) {
		t.Fatalf("err = %v, want ErrTLSFailed", err)
	}
	if server.hasCommand("AUTH") {
		t.Error("AUTH was issued after refused STARTTLS")
	}
}

func TestInvalidCertificate(t *testing.T) {
	cert, _ := testCertificate(t, nil, []net.IP{net.ParseIP("127.0.0.1")})
	server := newTestServer(t, testServerOptions{
		implicitTLS: true,
		requireAuth: true,
		username:    "user@example.test",
		password:    "s3cret",
		cert:        cert,
	})
	sender := newTestClient(server, nil) // no trust root
	_, err := sender.Send(context.Background(), testRequest(server, true))
	if !errors.Is(err, ErrTLSFailed) {
		t.Fatalf("err = %v, want ErrTLSFailed", err)
	}
	if server.hasCommand("AUTH") {
		t.Error("AUTH was issued before certificate verification")
	}
}

func TestWrongCertificateHostname(t *testing.T) {
	cert, pool := testCertificate(t, []string{"mail.example.test"}, nil)
	server := newTestServer(t, testServerOptions{
		implicitTLS: true,
		requireAuth: true,
		username:    "user@example.test",
		password:    "s3cret",
		cert:        cert,
	})
	req := testRequest(server, true)
	req.Config.Host = "wrong.example.test"
	sender := newTestClient(server, pool)
	_, err := sender.Send(context.Background(), req)
	if !errors.Is(err, ErrTLSFailed) {
		t.Fatalf("err = %v, want ErrTLSFailed", err)
	}
	if server.hasCommand("AUTH") {
		t.Error("AUTH was issued to a wrong-host certificate")
	}
}

func TestCredentialsNeverSentWithoutTLS(t *testing.T) {
	cert, pool := testCertificate(t, nil, []net.IP{net.ParseIP("127.0.0.1")})
	server := newTestServer(t, testServerOptions{
		startTLS:    false,
		requireAuth: true,
		username:    "user@example.test",
		password:    "s3cret",
		cert:        cert,
	})
	sender := newTestClient(server, pool)
	_, err := sender.Send(context.Background(), testRequest(server, false))
	if !errors.Is(err, ErrTLSFailed) {
		t.Fatalf("err = %v, want ErrTLSFailed", err)
	}
	_, _, _, authSeen, authBeforeTLS, _, _, _ := server.snapshot()
	if authSeen || authBeforeTLS {
		t.Error("credentials were transmitted without TLS")
	}
}

func TestMailFromRejected(t *testing.T) {
	cert, pool := testCertificate(t, nil, []net.IP{net.ParseIP("127.0.0.1")})
	server := newTestServer(t, testServerOptions{
		implicitTLS: true,
		requireAuth: true,
		username:    "user@example.test",
		password:    "s3cret",
		cert:        cert,
		mailCode:    550,
	})
	sender := newTestClient(server, pool)
	_, err := sender.Send(context.Background(), testRequest(server, true))
	if !errors.Is(err, ErrSenderRejected) {
		t.Fatalf("err = %v, want ErrSenderRejected", err)
	}
	if server.hasCommand("RCPT TO:") || server.hasCommand("DATA") {
		t.Error("transaction continued after MAIL FROM rejection")
	}
}

func TestFirstRecipientRejected(t *testing.T) {
	cert, pool := testCertificate(t, nil, []net.IP{net.ParseIP("127.0.0.1")})
	server := newTestServer(t, testServerOptions{
		implicitTLS: true,
		requireAuth: true,
		username:    "user@example.test",
		password:    "s3cret",
		cert:        cert,
		rcptFailAt:  1,
	})
	sender := newTestClient(server, pool)
	_, err := sender.Send(context.Background(), testRequest(server, true))
	if !errors.Is(err, ErrRecipientRejected) {
		t.Fatalf("err = %v, want ErrRecipientRejected", err)
	}
	_, _, rcpts, _, _, _, _, _ := server.snapshot()
	if len(rcpts) != 1 {
		t.Errorf("rcpts = %v, want only the rejected first recipient", rcpts)
	}
	if server.hasCommand("DATA") {
		t.Error("DATA followed a rejected first recipient")
	}
	if !server.hasCommand("RSET") {
		t.Error("RSET was not attempted after RCPT rejection")
	}
}

func TestLaterRecipientRejectedNoDATA(t *testing.T) {
	cert, pool := testCertificate(t, nil, []net.IP{net.ParseIP("127.0.0.1")})
	server := newTestServer(t, testServerOptions{
		implicitTLS: true,
		requireAuth: true,
		username:    "user@example.test",
		password:    "s3cret",
		cert:        cert,
		rcptFailAt:  2,
	})
	sender := newTestClient(server, pool)
	_, err := sender.Send(context.Background(), testRequest(server, true))
	if !errors.Is(err, ErrRecipientRejected) {
		t.Fatalf("err = %v, want ErrRecipientRejected", err)
	}
	_, _, rcpts, _, _, _, _, _ := server.snapshot()
	if len(rcpts) != 2 {
		t.Errorf("rcpts = %v, want both attempted recipients", rcpts)
	}
	if server.hasCommand("DATA") {
		t.Error("DATA followed a later recipient rejection")
	}
	if !server.hasCommand("RSET") {
		t.Error("RSET was not attempted after later RCPT rejection")
	}
}

func TestDATACommandRejected(t *testing.T) {
	cert, pool := testCertificate(t, nil, []net.IP{net.ParseIP("127.0.0.1")})
	server := newTestServer(t, testServerOptions{
		implicitTLS: true,
		requireAuth: true,
		username:    "user@example.test",
		password:    "s3cret",
		cert:        cert,
		dataCode:    554,
	})
	sender := newTestClient(server, pool)
	_, err := sender.Send(context.Background(), testRequest(server, true))
	if !errors.Is(err, ErrMessageRejected) {
		t.Fatalf("err = %v, want ErrMessageRejected", err)
	}
	var ambiguous *AmbiguousError
	if errors.As(err, &ambiguous) {
		t.Fatal("DATA command rejection was misclassified as ambiguous")
	}
	select {
	case <-server.dataReceived:
		t.Fatal("message data was transmitted after DATA rejection")
	default:
	}
}

func TestFinalDataRejected(t *testing.T) {
	cert, pool := testCertificate(t, nil, []net.IP{net.ParseIP("127.0.0.1")})
	server := newTestServer(t, testServerOptions{
		implicitTLS: true,
		requireAuth: true,
		username:    "user@example.test",
		password:    "s3cret",
		cert:        cert,
		finalCode:   554,
	})
	sender := newTestClient(server, pool)
	_, err := sender.Send(context.Background(), testRequest(server, true))
	if !errors.Is(err, ErrMessageRejected) {
		t.Fatalf("err = %v, want ErrMessageRejected", err)
	}
	var ambiguous *AmbiguousError
	if errors.As(err, &ambiguous) {
		t.Fatal("explicit final rejection was misclassified as ambiguous")
	}
	server.waitForData(t)
}

func TestDisconnectDuringDATAIsAmbiguous(t *testing.T) {
	cert, pool := testCertificate(t, nil, []net.IP{net.ParseIP("127.0.0.1")})
	server := newTestServer(t, testServerOptions{
		implicitTLS: true,
		requireAuth: true,
		username:    "user@example.test",
		password:    "s3cret",
		cert:        cert,
		dataMode:    dataCloseAfter354,
	})
	sender := newTestClient(server, pool)
	_, err := sender.Send(context.Background(), testRequest(server, true))
	assertAmbiguous(t, err, "test-id@example.test")
	_, _, _, _, _, _, _, accepts := server.snapshot()
	if accepts != 1 {
		t.Errorf("accepts = %d, want exactly one SMTP attempt", accepts)
	}
}

func TestDisconnectAfterDataIsAmbiguous(t *testing.T) {
	cert, pool := testCertificate(t, nil, []net.IP{net.ParseIP("127.0.0.1")})
	server := newTestServer(t, testServerOptions{
		implicitTLS: true,
		requireAuth: true,
		username:    "user@example.test",
		password:    "s3cret",
		cert:        cert,
		dataMode:    dataCloseAfterDot,
	})
	sender := newTestClient(server, pool)
	_, err := sender.Send(context.Background(), testRequest(server, true))
	assertAmbiguous(t, err, "test-id@example.test")
	server.waitForData(t)
	_, _, _, _, _, _, _, accepts := server.snapshot()
	if accepts != 1 {
		t.Errorf("accepts = %d, want exactly one SMTP attempt", accepts)
	}
}

func TestFinalResponseTimeoutIsAmbiguous(t *testing.T) {
	cert, pool := testCertificate(t, nil, []net.IP{net.ParseIP("127.0.0.1")})
	server := newTestServer(t, testServerOptions{
		implicitTLS: true,
		requireAuth: true,
		username:    "user@example.test",
		password:    "s3cret",
		cert:        cert,
		dataMode:    dataDelayAfterDot,
		dataDelay:   5 * time.Second,
	})
	sender := newTestClient(server, pool)
	sender.SubmissionTimeout = 50 * time.Millisecond
	_, err := sender.Send(context.Background(), testRequest(server, true))
	assertAmbiguous(t, err, "test-id@example.test")
	server.waitForData(t)
}

func TestCancellationDuringFinalResponseIsAmbiguous(t *testing.T) {
	cert, pool := testCertificate(t, nil, []net.IP{net.ParseIP("127.0.0.1")})
	server := newTestServer(t, testServerOptions{
		implicitTLS: true,
		requireAuth: true,
		username:    "user@example.test",
		password:    "s3cret",
		cert:        cert,
		dataMode:    dataDelayAfterDot,
		dataDelay:   5 * time.Second,
	})
	sender := newTestClient(server, pool)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type outcome struct {
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		_, err := sender.Send(ctx, testRequest(server, true))
		done <- outcome{err: err}
	}()
	server.waitForData(t)
	cancel()
	select {
	case got := <-done:
		assertAmbiguous(t, got.err, "test-id@example.test")
	case <-time.After(3 * time.Second):
		t.Fatal("Send did not return after context cancellation")
	}
}

func TestDataWriteTimeoutIsAmbiguous(t *testing.T) {
	cert, pool := testCertificate(t, nil, []net.IP{net.ParseIP("127.0.0.1")})
	server := newTestServer(t, testServerOptions{
		implicitTLS: true,
		requireAuth: true,
		username:    "user@example.test",
		password:    "s3cret",
		cert:        cert,
		dataMode:    dataNoRead,
	})
	sender := newTestClient(server, pool)
	sender.DataTimeout = 50 * time.Millisecond
	req := testRequest(server, true)
	req.Message = bytes.Repeat([]byte("x"), 16*1024*1024)
	_, err := sender.Send(context.Background(), req)
	assertAmbiguous(t, err, "test-id@example.test")
}

func TestConnectionTimeout(t *testing.T) {
	client := New()
	client.ConnectTimeout = 40 * time.Millisecond
	client.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	req := Request{
		Config:     config.SMTPConfig{Host: "127.0.0.1", Port: 2525, TLS: true, Username: "u", Password: "p"},
		From:       "a@example.test",
		Recipients: []string{"b@example.test"},
		Message:    []byte("x"),
		MessageID:  "id@example.test",
	}
	_, err := client.Send(context.Background(), req)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
}

func TestTLSHandshakeTimeout(t *testing.T) {
	cert, pool := testCertificate(t, nil, []net.IP{net.ParseIP("127.0.0.1")})
	server := newTestServer(t, testServerOptions{
		implicitTLS:      true,
		skipTLSHandshake: true,
		cert:             cert,
	})
	sender := newTestClient(server, pool)
	sender.TLSHandshakeTimeout = 50 * time.Millisecond
	_, err := sender.Send(context.Background(), testRequest(server, true))
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
}

func TestCommandTimeout(t *testing.T) {
	cert, pool := testCertificate(t, nil, []net.IP{net.ParseIP("127.0.0.1")})
	server := newTestServer(t, testServerOptions{
		implicitTLS: true,
		noEHLO:      true,
		cert:        cert,
	})
	sender := newTestClient(server, pool)
	sender.CommandTimeout = 50 * time.Millisecond
	_, err := sender.Send(context.Background(), testRequest(server, true))
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
}

func TestContextCancelledDuringCommand(t *testing.T) {
	cert, pool := testCertificate(t, nil, []net.IP{net.ParseIP("127.0.0.1")})
	server := newTestServer(t, testServerOptions{
		implicitTLS: true,
		noEHLO:      true,
		cert:        cert,
	})
	sender := newTestClient(server, pool)
	sender.CommandTimeout = 5 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	defer cancel()
	_, err := sender.Send(ctx, testRequest(server, true))
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
}

func TestContextCancelledBeforeDial(t *testing.T) {
	var dials int
	client := New()
	client.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		dials++
		return nil, errors.New("must not dial")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := Request{
		Config:     config.SMTPConfig{Host: "127.0.0.1", Port: 2525, TLS: true, Username: "u", Password: "p"},
		From:       "a@example.test",
		Recipients: []string{"b@example.test"},
		Message:    []byte("x"),
		MessageID:  "id@example.test",
	}
	_, err := client.Send(ctx, req)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if dials != 0 {
		t.Errorf("dials = %d, want 0", dials)
	}
}

func TestSMTPUTF8RejectedBeforeDial(t *testing.T) {
	var dials int
	client := New()
	client.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		dials++
		return nil, errors.New("must not dial")
	}
	req := Request{
		Config:     config.SMTPConfig{Host: "127.0.0.1", Port: 2525, TLS: true, Username: "u", Password: "p"},
		From:       "sénd@example.test",
		Recipients: []string{"b@example.test"},
		Message:    []byte("x"),
		MessageID:  "id@example.test",
	}
	_, err := client.Send(context.Background(), req)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	if dials != 0 {
		t.Errorf("dials = %d, want 0", dials)
	}
}

func TestPasswordCommand(t *testing.T) {
	cert, pool := testCertificate(t, nil, []net.IP{net.ParseIP("127.0.0.1")})
	server := newTestServer(t, testServerOptions{
		implicitTLS: true,
		requireAuth: true,
		username:    "user@example.test",
		password:    "s3cret",
		cert:        cert,
	})
	req := testRequest(server, true)
	req.Config.Password = ""
	req.Config.PasswordCommand = []string{"/bin/echo", "s3cret"}
	sender := newTestClient(server, pool)
	if _, err := sender.Send(context.Background(), req); err != nil {
		t.Fatalf("Send: %v", err)
	}
}

func TestPasswordCommandFailureDoesNotLeakOutput(t *testing.T) {
	client := New()
	client.CredentialTimeout = time.Second
	req := Request{
		Config: config.SMTPConfig{
			Host:            "127.0.0.1",
			Port:            2525,
			TLS:             true,
			Username:        "u",
			PasswordCommand: []string{"/bin/sh", "-c", "echo super-secret-output; exit 1"},
		},
		From:       "a@example.test",
		Recipients: []string{"b@example.test"},
		Message:    []byte("x"),
		MessageID:  "id@example.test",
	}
	_, err := client.Send(context.Background(), req)
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("err = %v, want ErrAuthFailed", err)
	}
	if strings.Contains(err.Error(), "super-secret-output") {
		t.Fatalf("error leaked command output: %v", err)
	}
}

func TestPasswordCommandTimeout(t *testing.T) {
	client := New()
	client.CredentialTimeout = 40 * time.Millisecond
	req := Request{
		Config: config.SMTPConfig{
			Host:            "127.0.0.1",
			Port:            2525,
			TLS:             true,
			Username:        "u",
			PasswordCommand: []string{"/bin/sleep", "5"},
		},
		From:       "a@example.test",
		Recipients: []string{"b@example.test"},
		Message:    []byte("x"),
		MessageID:  "id@example.test",
	}
	_, err := client.Send(context.Background(), req)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
}

func TestPasswordCommandCancellation(t *testing.T) {
	var dials int
	client := New()
	client.CredentialTimeout = 5 * time.Second
	client.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		dials++
		return nil, errors.New("must not dial")
	}
	req := Request{
		Config: config.SMTPConfig{
			Host:            "127.0.0.1",
			Port:            2525,
			TLS:             true,
			Username:        "u",
			PasswordCommand: []string{"/bin/sleep", "5"},
		},
		From:       "a@example.test",
		Recipients: []string{"b@example.test"},
		Message:    []byte("x"),
		MessageID:  "id@example.test",
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	defer cancel()
	_, err := client.Send(ctx, req)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if dials != 0 {
		t.Errorf("dials = %d, want 0", dials)
	}
}

func TestPasswordCommandOutputBounded(t *testing.T) {
	client := New()
	client.CredentialTimeout = time.Second
	req := Request{
		Config: config.SMTPConfig{
			Host:            "127.0.0.1",
			Port:            2525,
			TLS:             true,
			Username:        "u",
			PasswordCommand: []string{"/bin/sh", "-c", "yes x | head -c 100000"},
		},
		From:       "a@example.test",
		Recipients: []string{"b@example.test"},
		Message:    []byte("x"),
		MessageID:  "id@example.test",
	}
	_, err := client.Send(context.Background(), req)
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("err = %v, want ErrAuthFailed", err)
	}
}

func TestConcurrentAccounts(t *testing.T) {
	cert, pool := testCertificate(t, nil, []net.IP{net.ParseIP("127.0.0.1")})
	server := newTestServer(t, testServerOptions{
		implicitTLS: true,
		requireAuth: true,
		username:    "user@example.test",
		password:    "s3cret",
		cert:        cert,
	})
	sender := newTestClient(server, pool)

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i, id := range []string{"account-a", "account-b"} {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			req := testRequest(server, true)
			req.AccountID = id
			req.From = "sender" + id + "@example.test"
			req.MessageID = id + "@example.test"
			req.Message = []byte("From: " + req.From + "\r\n\r\nbody-" + id + "\r\n")
			if _, err := sender.Send(context.Background(), req); err != nil {
				errs <- err
			}
		}(i, id)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent Send: %v", err)
	}
	_, _, _, _, _, _, _, accepts := server.snapshot()
	if accepts != 2 {
		t.Errorf("accepts = %d, want 2", accepts)
	}
}

func TestNoRetryAfterAmbiguousFailure(t *testing.T) {
	cert, pool := testCertificate(t, nil, []net.IP{net.ParseIP("127.0.0.1")})
	server := newTestServer(t, testServerOptions{
		implicitTLS: true,
		requireAuth: true,
		username:    "user@example.test",
		password:    "s3cret",
		cert:        cert,
		dataMode:    dataCloseAfterDot,
	})
	sender := newTestClient(server, pool)
	_, err := sender.Send(context.Background(), testRequest(server, true))
	assertAmbiguous(t, err, "test-id@example.test")
	time.Sleep(100 * time.Millisecond)
	_, _, _, _, _, _, _, accepts := server.snapshot()
	if accepts != 1 {
		t.Errorf("accepts = %d, want no retry", accepts)
	}
}

func assertAmbiguous(t *testing.T, err error, wantID string) {
	t.Helper()
	if !errors.Is(err, ErrAmbiguousSubmission) {
		t.Fatalf("err = %v, want ErrAmbiguousSubmission", err)
	}
	var ambiguous *AmbiguousError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("err = %T, want *AmbiguousError", err)
	}
	if ambiguous.MessageID != wantID {
		t.Errorf("MessageID = %q, want %q", ambiguous.MessageID, wantID)
	}
}

func hasPrefix(values []string, prefix string) bool {
	for _, value := range values {
		if strings.HasPrefix(strings.ToUpper(value), prefix) {
			return true
		}
	}
	return false
}
