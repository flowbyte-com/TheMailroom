package smtpclient

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/textproto"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flowbyte-com/themailroom/internal/config"
)

type dataMode int

const (
	dataNormal dataMode = iota
	dataCloseAfter354
	dataCloseAfterDot
	dataDelayAfterDot
	dataNoRead
)

type testServerOptions struct {
	implicitTLS      bool
	startTLS         bool
	startTLSCode     int
	requireAuth      bool
	username         string
	password         string
	cert             tls.Certificate
	mailCode         int
	rcptFailAt       int
	dataCode         int
	finalCode        int
	dataMode         dataMode
	dataDelay        time.Duration
	skipTLSHandshake bool
	noEHLO           bool
}

type testServer struct {
	t    *testing.T
	ln   net.Listener
	opts testServerOptions

	accepts atomic.Int32

	mu            sync.Mutex
	conns         map[net.Conn]struct{}
	commands      []string
	mailFrom      string
	rcpts         []string
	authSeen      bool
	authBeforeTLS bool
	authPayload   []byte
	data          []byte

	dataStarted  chan struct{}
	dataReceived chan struct{}
	dataOnce     sync.Once
	receivedOnce sync.Once

	done     chan struct{}
	doneOnce sync.Once
	wg       sync.WaitGroup
}

func newTestServer(t *testing.T, opts testServerOptions) *testServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &testServer{
		t:            t,
		ln:           ln,
		opts:         opts,
		conns:        make(map[net.Conn]struct{}),
		dataStarted:  make(chan struct{}),
		dataReceived: make(chan struct{}),
		done:         make(chan struct{}),
	}
	s.wg.Add(1)
	go s.acceptLoop()
	t.Cleanup(s.close)
	return s
}

func (s *testServer) addr() string { return s.ln.Addr().String() }

func (s *testServer) close() {
	s.doneOnce.Do(func() { close(s.done) })
	_ = s.ln.Close()
	s.mu.Lock()
	for conn := range s.conns {
		_ = conn.Close()
	}
	s.mu.Unlock()
	wait := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(wait)
	}()
	select {
	case <-wait:
	case <-time.After(2 * time.Second):
	}
}

func (s *testServer) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.accepts.Add(1)
		s.mu.Lock()
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.conns, conn)
				s.mu.Unlock()
				_ = conn.Close()
			}()
			s.handle(conn)
		}()
	}
}

func (s *testServer) handle(raw net.Conn) {
	conn := raw
	tlsActive := false
	if s.opts.implicitTLS {
		if s.opts.skipTLSHandshake {
			_, _ = io.Copy(io.Discard, raw)
			return
		}
		tlsConn := tls.Server(raw, s.serverTLSConfig())
		if err := tlsConn.Handshake(); err != nil {
			return
		}
		conn = tlsConn
		tlsActive = true
	}

	tp := textproto.NewReader(bufio.NewReader(conn))
	bw := bufio.NewWriter(conn)
	tw := textproto.NewWriter(bw)
	if err := tw.PrintfLine("220 test ESMTP ready"); err != nil {
		return
	}
	if err := bw.Flush(); err != nil {
		return
	}

	authenticated := false
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		s.record(line)
		upper := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(upper, "EHLO "):
			if s.opts.noEHLO {
				<-s.done
				return
			}
			if err := s.writeEHLO(tw, tlsActive); err != nil {
				return
			}
			_ = bw.Flush()
		case strings.HasPrefix(upper, "HELO "):
			_ = tw.PrintfLine("250 test")
			_ = bw.Flush()
		case upper == "STARTTLS":
			if s.opts.startTLSCode != 0 {
				_ = tw.PrintfLine("%d 4.7.0 STARTTLS refused", s.opts.startTLSCode)
				_ = bw.Flush()
				continue
			}
			if !s.opts.startTLS || tlsActive {
				_ = tw.PrintfLine("454 4.7.0 TLS not available")
				_ = bw.Flush()
				continue
			}
			_ = tw.PrintfLine("220 2.0.0 Ready to start TLS")
			_ = bw.Flush()
			if s.opts.skipTLSHandshake {
				_, _ = io.Copy(io.Discard, conn)
				return
			}
			tlsConn := tls.Server(conn, s.serverTLSConfig())
			if err := tlsConn.Handshake(); err != nil {
				return
			}
			conn = tlsConn
			tlsActive = true
			tp = textproto.NewReader(bufio.NewReader(conn))
			bw = bufio.NewWriter(conn)
			tw = textproto.NewWriter(bw)
		case strings.HasPrefix(upper, "AUTH "):
			s.recordAuth(line, tlsActive)
			if !tlsActive {
				_ = tw.PrintfLine("538 5.7.11 Encryption required for requested authentication mechanism")
				_ = bw.Flush()
				continue
			}
			if s.authenticate(tp, tw, bw, line) {
				authenticated = true
			}
		case strings.HasPrefix(upper, "MAIL FROM:"):
			s.recordMail(line)
			if s.opts.requireAuth && !authenticated {
				_ = tw.PrintfLine("530 5.7.0 Authentication required")
			} else if s.opts.mailCode != 0 {
				_ = tw.PrintfLine("%d 5.1.0 Sender rejected", s.opts.mailCode)
			} else {
				_ = tw.PrintfLine("250 2.1.0 Sender OK")
			}
			_ = bw.Flush()
		case strings.HasPrefix(upper, "RCPT TO:"):
			s.recordRcpt(line)
			s.mu.Lock()
			n := len(s.rcpts)
			s.mu.Unlock()
			if s.opts.rcptFailAt > 0 && n == s.opts.rcptFailAt {
				_ = tw.PrintfLine("550 5.1.1 Recipient rejected")
			} else {
				_ = tw.PrintfLine("250 2.1.5 Recipient OK")
			}
			_ = bw.Flush()
		case upper == "DATA":
			if s.opts.dataCode != 0 {
				_ = tw.PrintfLine("%d 5.6.0 Message rejected before data", s.opts.dataCode)
				_ = bw.Flush()
				continue
			}
			_ = tw.PrintfLine("354 End data with <CR><LF>.<CR><LF>")
			_ = bw.Flush()
			s.dataOnce.Do(func() { close(s.dataStarted) })

			switch s.opts.dataMode {
			case dataCloseAfter354:
				return
			case dataNoRead:
				select {
				case <-s.done:
				case <-time.After(10 * time.Second):
				}
				return
			default:
				data, err := io.ReadAll(tp.DotReader())
				if err != nil {
					return
				}
				s.mu.Lock()
				s.data = data
				s.mu.Unlock()
				s.receivedOnce.Do(func() { close(s.dataReceived) })
				if s.opts.dataMode == dataCloseAfterDot {
					return
				}
				if s.opts.dataMode == dataDelayAfterDot && s.opts.dataDelay > 0 {
					select {
					case <-s.done:
						return
					case <-time.After(s.opts.dataDelay):
					}
				}
				if s.opts.finalCode != 0 {
					_ = tw.PrintfLine("%d 5.6.0 Message rejected", s.opts.finalCode)
				} else {
					_ = tw.PrintfLine("250 2.0.0 Message accepted for delivery")
				}
				_ = bw.Flush()
			}
		case upper == "RSET":
			_ = tw.PrintfLine("250 2.0.0 Reset")
			_ = bw.Flush()
		case upper == "NOOP":
			_ = tw.PrintfLine("250 2.0.0 OK")
			_ = bw.Flush()
		case upper == "QUIT":
			_ = tw.PrintfLine("221 2.0.0 Bye")
			_ = bw.Flush()
			return
		default:
			_ = tw.PrintfLine("500 5.5.2 Command unrecognized")
			_ = bw.Flush()
		}
	}
}

func (s *testServer) writeEHLO(tw *textproto.Writer, tlsActive bool) error {
	if err := tw.PrintfLine("250-test"); err != nil {
		return err
	}
	if s.opts.startTLS && !tlsActive {
		if err := tw.PrintfLine("250-STARTTLS"); err != nil {
			return err
		}
	}
	if s.opts.requireAuth {
		if err := tw.PrintfLine("250-AUTH PLAIN"); err != nil {
			return err
		}
	}
	return tw.PrintfLine("250 SIZE 104857600")
}

func (s *testServer) authenticate(tp *textproto.Reader, tw *textproto.Writer, bw *bufio.Writer, line string) bool {
	parts := strings.SplitN(strings.TrimSpace(line), " ", 3)
	if len(parts) < 2 || !strings.EqualFold(parts[1], "PLAIN") {
		_ = tw.PrintfLine("504 5.5.4 Unsupported authentication mechanism")
		_ = bw.Flush()
		return false
	}
	var payload string
	if len(parts) == 3 {
		payload = parts[2]
	} else {
		_ = tw.PrintfLine("334 ")
		_ = bw.Flush()
		line, err := tp.ReadLine()
		if err != nil {
			return false
		}
		payload = line
	}
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		_ = tw.PrintfLine("501 5.5.2 Invalid base64")
		_ = bw.Flush()
		return false
	}
	s.mu.Lock()
	s.authPayload = append([]byte(nil), decoded...)
	s.mu.Unlock()

	fields := strings.Split(string(decoded), "\x00")
	if len(fields) != 3 || fields[1] != s.opts.username || fields[2] != s.opts.password {
		_ = tw.PrintfLine("535 5.7.8 Authentication credentials invalid")
		_ = bw.Flush()
		return false
	}
	_ = tw.PrintfLine("235 2.7.0 Authentication successful")
	_ = bw.Flush()
	return true
}

func (s *testServer) record(line string) {
	s.mu.Lock()
	s.commands = append(s.commands, line)
	s.mu.Unlock()
}

func (s *testServer) recordAuth(line string, tlsActive bool) {
	s.mu.Lock()
	s.authSeen = true
	s.authBeforeTLS = !tlsActive
	s.mu.Unlock()
}

func (s *testServer) recordMail(line string) {
	s.mu.Lock()
	s.mailFrom = envelopeAddress(line)
	s.mu.Unlock()
}

func (s *testServer) recordRcpt(line string) {
	s.mu.Lock()
	s.rcpts = append(s.rcpts, envelopeAddress(line))
	s.mu.Unlock()
}

func (s *testServer) serverTLSConfig() *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{s.opts.cert},
		MinVersion:   tls.VersionTLS12,
	}
}

func envelopeAddress(line string) string {
	start := strings.IndexByte(line, '<')
	end := strings.LastIndexByte(line, '>')
	if start >= 0 && end > start {
		return line[start+1 : end]
	}
	return ""
}

func (s *testServer) waitForData(t *testing.T) []byte {
	t.Helper()
	select {
	case <-s.dataReceived:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for SMTP DATA")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.data...)
}

func (s *testServer) snapshot() (commands []string, mailFrom string, rcpts []string, authSeen, authBeforeTLS bool, authPayload, data []byte, accepts int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commands...), s.mailFrom, append([]string(nil), s.rcpts...),
		s.authSeen, s.authBeforeTLS, append([]byte(nil), s.authPayload...), append([]byte(nil), s.data...),
		int(s.accepts.Load())
}

func (s *testServer) hasCommand(prefix string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, command := range s.commands {
		if strings.HasPrefix(strings.ToUpper(command), prefix) {
			return true
		}
	}
	return false
}

func testCertificate(t *testing.T, dnsNames []string, ips []net.IP) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-smtp"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(2 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: cert}, pool
}

func dialTo(address string) func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{}
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, address)
	}
}

func newTestClient(server *testServer, pool *x509.CertPool) *Client {
	c := New()
	c.RootCAs = pool
	c.DialContext = dialTo(server.addr())
	c.ConnectTimeout = 2 * time.Second
	c.TLSHandshakeTimeout = 2 * time.Second
	c.CommandTimeout = 2 * time.Second
	c.DataTimeout = 2 * time.Second
	c.SubmissionTimeout = 2 * time.Second
	c.CredentialTimeout = 2 * time.Second
	return c
}

func testRequest(server *testServer, implicitTLS bool) Request {
	host, port, err := net.SplitHostPort(server.addr())
	if err != nil {
		panic(fmt.Sprintf("split addr: %v", err))
	}
	var p int
	_, _ = fmt.Sscanf(port, "%d", &p)
	cfg := config.SMTPConfig{
		Host:     host,
		Port:     p,
		Username: "user@example.test",
		Password: "s3cret",
	}
	if implicitTLS {
		cfg.TLS = true
	} else {
		cfg.StartTLS = true
	}
	return Request{
		AccountID:  "test",
		Config:     cfg,
		From:       "Sales@Example.TEST",
		Recipients: []string{"alice@example.test", "Bob@Example.TEST"},
		Message:    []byte("From: Sales@example.test\r\nTo: alice@example.test\r\nSubject: test\r\n\r\nbody\r\n"),
		MessageID:  "test-id@example.test",
	}
}
