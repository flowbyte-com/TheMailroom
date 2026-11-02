package tools

import (
	"context"

	"github.com/flowbyte-com/themailroom/internal/mailbox"
)

// unknownAttachmentBackend wraps a backend and clears the attachment
// presence of every summary it returns.
//
// It reproduces the IMAP list and search behaviour, where the client
// fetches envelopes only and therefore does not determine whether a
// message has attachments. The wrapper exists so the wire shape for an
// unknown answer can be asserted deterministically, without depending
// on an IMAP server.
type unknownAttachmentBackend struct {
	mailbox.Backend
}

func (b *unknownAttachmentBackend) ListMessages(ctx context.Context, accountID, mbx string, opts mailbox.ListOptions) (mailbox.ListResult, error) {
	res, err := b.Backend.ListMessages(ctx, accountID, mbx, opts)
	if err != nil {
		return res, err
	}
	res.Messages = clearAttachmentPresence(res.Messages)
	return res, nil
}

func (b *unknownAttachmentBackend) SearchMessages(ctx context.Context, accountID, mbx string, q mailbox.SearchQuery) (mailbox.ListResult, error) {
	res, err := b.Backend.SearchMessages(ctx, accountID, mbx, q)
	if err != nil {
		return res, err
	}
	res.Messages = clearAttachmentPresence(res.Messages)
	return res, nil
}

// clearAttachmentPresence sets every summary's attachment presence to
// unknown.
func clearAttachmentPresence(in []mailbox.MessageSummary) []mailbox.MessageSummary {
	out := make([]mailbox.MessageSummary, len(in))
	copy(out, in)
	for i := range out {
		out[i].HasAttachment = nil
	}
	return out
}
