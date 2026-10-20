package imapclient

// IMAP search support.
//
// The IMAP SEARCH command cannot express the conjunction of
// several *different* header fields in a server-independent way:
// a single search key may be repeated (and repetitions are ORed
// by most servers), and the generic TEXT key matches anywhere in
// the headers or the body.
//
// To give the caller AND semantics across the header criteria
// without depending on server-specific extensions, the backend
// groups the criteria and evaluates them as follows:
//
//   - Each populated header criterion forms a *group*. A message
//     must match every group (AND across groups).
//   - Within a group, the terms are alternatives and a message
//     matches the group if it matches any of them (OR within a
//     group). The "to" group has two terms, TO and CC, because
//     the public API documents the To filter as matching any
//     recipient in To or Cc.
//
// The body, flag and date criteria are single-key criteria and
// are combined into every search, because the server ANDs
// different search keys.
//
// This is deliberately narrow: no provider-specific extensions
// (such as Gmail's X-GM-RAW) are used, so the behaviour is the
// same on every IMAP4rev1 server.

import (
	"fmt"
	"net/textproto"
	"sort"

	"github.com/flowbyte-com/go-imap"
	"github.com/flowbyte-com/go-imap/client"

	"github.com/flowbyte-com/themailroom/internal/mailbox"
	"github.com/flowbyte-com/themailroom/internal/searchspec"
)

// searchTerm is one alternative inside a search group.
type searchTerm struct {
	// name is a human-readable label, used in error messages.
	name string
	// header is the IMAP header search key (e.g. "Subject").
	// An empty header means the term adds no constraint
	// beyond the base criteria.
	header string
	// value is the substring to look for in the header.
	value string
}

// searchGroup is a set of alternative terms; a message matches
// the group if it matches any of the terms.
type searchGroup struct {
	name  string
	terms []searchTerm
}

// searchGroups returns the header criteria that must all match
// (AND across groups).
//
// The order is deterministic so that the sequence of SEARCH
// commands a server sees is stable, which keeps the integration
// tests deterministic.
func searchGroups(q mailbox.SearchQuery) []searchGroup {
	var groups []searchGroup
	if q.Subject != "" {
		groups = append(groups, searchGroup{
			name:  "subject",
			terms: []searchTerm{{name: "subject", header: "Subject", value: q.Subject}},
		})
	}
	if q.From != "" {
		groups = append(groups, searchGroup{
			name:  "from",
			terms: []searchTerm{{name: "from", header: "From", value: q.From}},
		})
	}
	if q.To != "" {
		// The public API documents the To filter as matching
		// any recipient in To or Cc, so both headers are
		// searched and their results are unioned.
		groups = append(groups, searchGroup{
			name: "to",
			terms: []searchTerm{
				{name: "to", header: "To", value: q.To},
				{name: "cc", header: "Cc", value: q.To},
			},
		})
	}
	return groups
}

// baseCriteria returns the criteria that apply to every search:
// the body term, the flag terms and the date terms.
//
// The query date terms are truncated to their UTC day boundary
// because the IMAP search date is a date with no time (see
// internal/searchspec). The IMAP server compares the message's
// Date: header by the calendar day named by its own fields, so a
// message sent earlier on the same local day matches a "since"
// of that day. The memory backend applies the same two rules,
// so the two backends agree.
func baseCriteria(q mailbox.SearchQuery) *imap.SearchCriteria {
	crit := &imap.SearchCriteria{}
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

// criteriaFor builds the criteria for one search term: the base
// criteria plus the term's header constraint. The two use
// different search keys, so the server ANDs them.
func criteriaFor(base *imap.SearchCriteria, term searchTerm) *imap.SearchCriteria {
	out := &imap.SearchCriteria{
		Body:         base.Body,
		WithFlags:    base.WithFlags,
		WithoutFlags: base.WithoutFlags,
		SentSince:    base.SentSince,
		SentBefore:   base.SentBefore,
	}
	if term.header != "" {
		out.Header = textproto.MIMEHeader{term.header: {term.value}}
	}
	return out
}

// searchUIDs runs the searches and returns the UIDs that match
// every group, in ascending order.
//
// The intersection is computed in the client rather than by the
// server so the AND semantics hold regardless of how a given
// server treats repeated search keys.
func searchUIDs(c *client.Client, q mailbox.SearchQuery) ([]uint32, error) {
	base := baseCriteria(q)
	groups := searchGroups(q)

	// No header criteria: a single search with the base
	// criteria is enough. An entirely empty query searches
	// everything, which is the documented behaviour.
	if len(groups) == 0 {
		uids, err := c.UidSearch(base)
		if err != nil {
			return nil, err
		}
		return uids, nil
	}

	// Evaluate each group, then intersect.
	var intersection map[uint32]struct{}
	for _, g := range groups {
		// A message matches the group if it matches any
		// term, so the group's UID set is the union of
		// its terms' UID sets.
		groupSet := make(map[uint32]struct{})
		for _, term := range g.terms {
			crit := criteriaFor(base, term)
			uids, err := c.UidSearch(crit)
			if err != nil {
				return nil, fmt.Errorf("imap: search by %s: %w", term.name, err)
			}
			for _, uid := range uids {
				groupSet[uid] = struct{}{}
			}
		}
		if intersection == nil {
			intersection = groupSet
			continue
		}
		for uid := range intersection {
			if _, ok := groupSet[uid]; !ok {
				delete(intersection, uid)
			}
		}
	}
	out := make([]uint32, 0, len(intersection))
	for uid := range intersection {
		out = append(out, uid)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}
