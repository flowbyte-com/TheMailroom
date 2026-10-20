package mailcompose

import (
	"bytes"
	"mime"
	"strings"
	"testing"

	"github.com/flowbyte-com/themailroom/internal/attachments"
	"github.com/flowbyte-com/themailroom/internal/mimeparse"
)

func TestLongFilenameRoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		filename string
	}{
		{name: "long ASCII", filename: strings.Repeat("a", 200) + ".txt"},
		{name: "long UTF-8", filename: strings.Repeat("é", 100) + ".txt"},
		{name: "short with spaces", filename: strings.Repeat("a ", 20) + "file.txt"},
		{name: "long with spaces", filename: strings.Repeat("a ", 100) + "file.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := baseRequest()
			req.Attachments = []attachments.File{{Filename: tc.filename, Data: []byte("data")}}
			result := mustCompose(t, req, testLimits)
			decoded := parseDecoded(t, result.Message)
			if len(decoded.Attachments) != 1 {
				t.Fatalf("attachments = %+v", decoded.Attachments)
			}
			if decoded.Attachments[0].Filename != tc.filename {
				t.Errorf("filename = %q, want %q", decoded.Attachments[0].Filename, tc.filename)
			}
			content, err := mimeparse.ExtractAttachment(result.Message, decoded.Attachments[0].Part, parseLimits)
			if err != nil {
				t.Fatal(err)
			}
			if string(content.Content) != "data" {
				t.Errorf("content = %q", content.Content)
			}
		})
	}
}

func TestLongSubjectRoundTrip(t *testing.T) {
	for _, subject := range []string{
		strings.Repeat("a", 1000),
		strings.Repeat("é", 200),
		strings.Repeat("a", 500) + " " + strings.Repeat("b", 500),
		strings.Repeat("a", 1000) + " tail",
		strings.Repeat("a  ", 400),
	} {
		req := baseRequest()
		req.Subject = subject
		result := mustCompose(t, req, testLimits)
		decoded := parseDecoded(t, result.Message)
		if decoded.Headers.Subject != subject {
			t.Errorf("Subject length = %d, want %d", len(decoded.Headers.Subject), len(subject))
		}
		for _, line := range bytes.Split(result.Message, []byte("\r\n")) {
			if len(line) > 998 {
				t.Fatalf("serialized line is %d bytes, exceeds RFC 5322 limit", len(line))
			}
		}
	}
}

func TestVeryLongFilenameUsesRFC2231Continuations(t *testing.T) {
	filename := strings.Repeat("a", 1000) + ".txt"
	value, err := contentDispositionValue(filename)
	if err != nil {
		t.Fatal(err)
	}
	disposition, params, err := mime.ParseMediaType(value)
	if err != nil {
		t.Fatal(err)
	}
	if disposition != "attachment" {
		t.Errorf("disposition = %q", disposition)
	}
	if params["filename"] != filename {
		t.Errorf("filename length = %d, want %d", len(params["filename"]), len(filename))
	}
	if !strings.Contains(value, "filename*0*=") {
		t.Error("long filename did not use RFC 2231 continuations")
	}

	req := baseRequest()
	req.Attachments = []attachments.File{{Filename: filename, Data: []byte("data")}}
	result := mustCompose(t, req, testLimits)
	if !bytes.Contains(result.Message, []byte("filename*0*=")) {
		t.Error("serialized message did not use RFC 2231 continuations")
	}
	decoded := parseDecoded(t, result.Message)
	if len(decoded.Attachments) != 1 {
		t.Fatalf("attachments = %+v", decoded.Attachments)
	}
	if want := filename[:255]; decoded.Attachments[0].Filename != want {
		t.Errorf("parsed filename length = %d, want %d", len(decoded.Attachments[0].Filename), len(want))
	}
	for _, line := range bytes.Split(result.Message, []byte("\r\n")) {
		if len(line) > 998 {
			t.Fatalf("serialized line is %d bytes, exceeds RFC 5322 limit", len(line))
		}
	}
}
