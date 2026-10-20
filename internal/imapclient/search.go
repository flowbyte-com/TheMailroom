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
	"net/textproto"

	"github.com/flowbyte-com/go-imap"
	"github.com/flowbyte-com/go-imap/client"

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
func searchUIDs(c *client.Client, q mailbox.SearchQuery) ([]uint32, error) {
	return c.UidSearch(searchCriteria(q))
}
