package imapclient

// IMAP literal pre-allocation cap.
//
// The actual filter implementation lives in literal_filter.go.
// This file holds the package-level documentation that
// connects the design rationale to the threat model.
//
// The literalLimitConn in literal_filter.go wraps the
// underlying net.Conn for the *implicit-TLS* path
// (cfg.IMAP.TLS). For the *STARTTLS* path
// (cfg.IMAP.StartTLS), the literal filter cannot be inserted
// around the upgraded TLS conn without re-implementing the
// STARTTLS handshake; the STARTTLS path therefore relies on
// the application-level io.LimitReader cap as a backstop.
//
// Why a pre-allocation cap matters
// ---------------------------------
// The go-imap client (v1.2.1) allocates a buffer of the
// literal size and reads the literal into it before the
// application sees the data
// (read.go::ReadLiteral). The application-level cap is a
// backstop, not a pre-allocation guard. The literalLimitConn
// is the pre-allocation mechanism: it observes the literal
// size announcement in the byte stream and, when the
// announced size exceeds the cap, closes the conn before the
// go-imap client allocates the buffer.
//
// Compatibility
// --------------
// The literalLimitConn does not modify the bytes the go-imap
// parser sees; it short-circuits the read with io.EOF. The
// go-imap parser observes an io.EOF where it expected
// literal data, which is the documented behaviour for an
// unexpectedly closed connection. The executor's errClosed
// path closes the connection and tears down the client, so
// the next call reconnects cleanly.
//
// A known limitation: a malicious server that announces a
// literal header (`{N}\r\n`) and never sends a newline is
// not possible because the literal header itself contains
// the newline. A malicious server that never sends the
// declared N bytes is handled by the go-imap client (the
// io.ReadFull fails with io.ErrUnexpectedEOF). The filter
// does not introduce new DoS surfaces.
//
// Tests for the filter are in correctness_test.go
// (TestLiteralLimitConnBoundary) and the integration
// tests for FetchMessageBodyCap.
