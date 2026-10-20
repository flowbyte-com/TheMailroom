package mailcompose

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/flowbyte-com/themailroom/internal/attachments"
)

func TestHeaderPreparationRejectsOversizedValues(t *testing.T) {
	limits := testLimits
	limits.MaxMessageBytes = 128

	aggregate := baseRequest()
	for i := 0; i < 20; i++ {
		addr := fmt.Sprintf("recipient-%02d@example.test", i)
		if i < 10 {
			aggregate.To = append(aggregate.To, addr)
		} else {
			aggregate.Cc = append(aggregate.Cc, addr)
		}
	}

	tests := []struct {
		name string
		req  Request
	}{
		{
			name: "oversized ASCII subject",
			req: func() Request {
				req := baseRequest()
				req.Subject = strings.Repeat("a", 1<<20)
				return req
			}(),
		},
		{
			name: "oversized internationalized subject",
			req: func() Request {
				req := baseRequest()
				req.Subject = strings.Repeat("é", 1<<19)
				return req
			}(),
		},
		{
			name: "oversized RFC 2231 filename",
			req: func() Request {
				req := baseRequest()
				req.Attachments = []attachments.File{{
					Filename: strings.Repeat("é", 1<<19) + ".txt",
					Data:     []byte("x"),
				}}
				return req
			}(),
		},
		{
			name: "aggregate address headers",
			req:  aggregate,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := fixedCompose(t, tc.req, limits)
			if !errors.Is(err, ErrMessageTooLarge) {
				t.Fatalf("err = %v, want ErrMessageTooLarge", err)
			}
			if len(result.Message) != 0 || result.MessageID != "" {
				t.Errorf("failure returned partial result: %+v", result)
			}
		})
	}
}

// TestOversizedSubjectRejectedBeforeEncoding guards against moving the size
// check after RFC 2047 encoding. QEncoding on a 1 MiB internationalized
// subject expands to several megabytes; the preflight check should reject it
// with only the small error path allocated.
func TestOversizedSubjectRejectedBeforeEncoding(t *testing.T) {
	req := baseRequest()
	req.Subject = strings.Repeat("é", 1<<19)
	limits := testLimits
	limits.MaxMessageBytes = 128

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	result, err := compose(req, limits, func() time.Time { return fixedTime }, func(_ time.Time, domain string) (string, error) {
		return "fixed-id@" + domain, nil
	})
	runtime.ReadMemStats(&after)

	if !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("err = %v, want ErrMessageTooLarge", err)
	}
	if len(result.Message) != 0 || result.MessageID != "" {
		t.Errorf("failure returned partial result: %+v", result)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 1<<20 {
		t.Fatalf("oversized subject allocated %d bytes before rejection", allocated)
	}
}
