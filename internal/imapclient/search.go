package imapclient

// IMAP search support.
//
// RFC 3501 section 6.4.4 ANDs multiple search keys and provides
// the OR search key for alternatives. The backend therefore
// builds one SearchCriteria: the Subject, From, Body, flag and
// date criteria are ANDed by the server, and the recipient
// filter is expressed as OR To Cc because the public API
// documents it as matching any recipient in either header.
//
// This is deliberately narrow: no provider-specific extensions
// (such as Gmail's X-GM-RAW) are used, so the behaviour is the
// same on every IMAP4rev1 server.

import (
	"errors"
	"fmt"
	"net/textproto"

	"github.com/flowbyte-com/go-imap"
	"github.com/flowbyte-com/go-imap/client"
	"github.com/flowbyte-com/go-imap/commands"
	"github.com/flowbyte-com/go-imap/responses"

	"github.com/flowbyte-com/themailroom/internal/config"
	"github.com/flowbyte-com/themailroom/internal/mailbox"
	"github.com/flowbyte-com/themailroom/internal/searchspec"
)

// searchCriteria translates the public query into one IMAP
// SEARCH. Different keys are ANDed by the server; q.To is an
// explicit OR over To and Cc because the public API matches
// either recipient header.
//
// The query date terms are truncated to their UTC day boundary
// because the IMAP search date is a date with no time (see
// internal/searchspec). The IMAP server compares the message's
// Date: header by the calendar day named by its own fields, so a
// message sent earlier on the same local day matches a "since"
// of that day. The memory backend applies the same two rules,
// so the two backends agree.
func searchCriteria(q mailbox.SearchQuery) *imap.SearchCriteria {
	crit := &imap.SearchCriteria{
		Header: make(textproto.MIMEHeader),
	}
	if q.Subject != "" {
		crit.Header.Set("Subject", q.Subject)
	}
	if q.From != "" {
		crit.Header.Set("From", q.From)
	}
	if q.To != "" {
		to := &imap.SearchCriteria{
			Header: textproto.MIMEHeader{"To": []string{q.To}},
		}
		cc := &imap.SearchCriteria{
			Header: textproto.MIMEHeader{"Cc": []string{q.To}},
		}
		crit.Or = [][2]*imap.SearchCriteria{{to, cc}}
	}
	if q.Body != "" {
		crit.Body = []string{q.Body}
	}
	if q.Flagged {
		crit.WithFlags = []string{searchspec.FlagFlagged}
	}
	if q.Unseen {
		crit.WithoutFlags = []string{searchspec.FlagSeen}
	}
	if q.Since != nil {
		crit.SentSince = searchspec.TruncateDate(*q.Since)
	}
	if q.Before != nil {
		crit.SentBefore = searchspec.TruncateDate(*q.Before)
	}
	return crit
}

// searchUIDs runs the single UID SEARCH and returns matching
// UIDs. Search errors are propagated to the caller.
//
// The budget bounds how many UIDs one search may examine. When the
// server returns more UIDs than the budget allows, the search fails:
// the application keeps no UIDs, issues no FETCH and returns a typed
// error rather than a partial result.
//
// The command is built and issued here rather than through
// client.UidSearch, because the fork's own handler always materialises
// the complete result: responses.Search.Handle allocates one uint32 per
// response field and client.UidSearch hands the finished slice to the
// caller. The collector below lets the application decide how much of
// the result it keeps. It does not bound the parser's own allocation,
// which happens before any handler runs; see docs/THREAT_MODEL.md for
// that residual.
func searchUIDs(c *client.Client, q mailbox.SearchQuery, budget int) ([]uint32, error) {
	crit := searchCriteria(q)
	col := &uidCollector{limit: budget}
	status, err := c.Execute(uidSearchCommand(crit, "UTF-8"), col)
	if status != nil && status.Code == imap.CodeBadCharset {
		// Some servers don't support UTF-8. The retry is a second
		// command, exactly as the go-imap client does it. The
		// collector is reset so the first attempt's UIDs are never
		// mixed with the second attempt's.
		col = &uidCollector{limit: budget}
		status, err = c.Execute(uidSearchCommand(crit, "US-ASCII"), col)
	}
	if err != nil {
		return nil, err
	}
	if status == nil {
		// The executor returns a status with every successful command.
		// Guard anyway: a nil status would be a crash, not an error.
		return nil, errors.New("imap: no status response for the search")
	}
	if err := status.Err(); err != nil {
		return nil, err
	}
	if col.overrun {
		return nil, searchTooLargeError(budget)
	}
	return col.uids, nil
}

// uidSearchCommand builds the UID SEARCH command for the criteria. It
// mirrors the command the go-imap client builds for client.UidSearch.
func uidSearchCommand(crit *imap.SearchCriteria, charset string) imap.Commander {
	var cmd imap.Commander = &commands.Search{Charset: charset, Criteria: crit}
	return &commands.Uid{Cmd: cmd}
}

// searchResponseName is the name of the untagged response that carries
// a SEARCH result.
const searchResponseName = "SEARCH"

// uidCollector collects the UIDs of a UID SEARCH response.
//
// It is a responses.Handler: the go-imap reader calls it with the
// parsed response. The limit bounds what the application keeps and
// therefore what it fetches; overrun records that the server returned
// more UIDs than the limit allows, so the caller can report the
// actual observation rather than a silent truncation.
//
// The parser has already materialised the whole response when the
// handler runs, so the collector cannot bound that allocation. It
// bounds everything downstream of it.
type uidCollector struct {
	// limit is the maximum number of UIDs the collector keeps.
	limit int
	// uids holds the retained UIDs in the order the server sent them.
	uids []uint32
	// overrun records that the server returned more UIDs than the
	// limit allows.
	overrun bool
}

// Handle implements responses.Handler.
//
// A response that is not a SEARCH result is left unhandled, so the
// executor's own tagged-status handling stays in charge.
func (c *uidCollector) Handle(resp imap.Resp) error {
	name, fields, ok := imap.ParseNamedResp(resp)
	if !ok || name != searchResponseName {
		return responses.ErrUnhandled
	}
	for _, f := range fields {
		id, err := imap.ParseNumber(f)
		if err != nil {
			return err
		}
		if len(c.uids) >= c.limit {
			// The server returned more UIDs than the budget allows.
			// The UIDs are still parsed, so a malformed field is
			// reported the same way the fork's handler reports it,
			// but they are not retained.
			c.overrun = true
			continue
		}
		c.uids = append(c.uids, id)
	}
	return nil
}

// searchTooLargeError reports that a search matched more messages than
// the scan budget allows.
//
// The recourse is to narrow the query or to raise
// limits.max_list_scan_messages, subject to its hard maximum. Narrowing
// the query does help here, unlike in the listing case: the search
// criteria are evaluated by the server, so a narrower query matches
// fewer messages and therefore examines fewer.
func searchTooLargeError(budget int) error {
	return fmt.Errorf("%w: the search matched more than %d messages, which is the scan budget; "+
		"narrow the query or raise limits.max_list_scan_messages (maximum %d) to run it",
		mailbox.ErrSearchTooLarge, budget, config.MaxListScanMessagesLimit)
}
