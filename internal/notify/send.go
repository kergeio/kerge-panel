package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Timeout bounds one request to the webhook, including reading the reply.
const Timeout = 10 * time.Second

// maxReplyBytes is how much of a reply body is kept.
const maxReplyBytes = 64 << 10

// Reply is what the webhook answered.
type Reply struct {
	Code int
	// Headers holds one "Name: value" line per header value, sorted by
	// name.
	Headers string
	// Body is the reply body, indented when it is JSON. It is cut at
	// 64 KiB.
	Body string
}

// OK reports whether the webhook accepted the message.
func (r Reply) OK() bool { return r.Code >= 200 && r.Code <= 299 }

// Sender posts messages to a webhook.
type Sender struct {
	client *http.Client
}

// NewSender returns a sender that gives up after Timeout and does not
// follow redirects, so a redirect is reported as the reply it is.
func NewSender() *Sender {
	return &Sender{client: &http.Client{
		Timeout: Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

// Send posts payload as JSON to target. An error means there was no reply.
func (s *Sender) Send(ctx context.Context, target, payload string) (Reply, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(payload))
	if err != nil {
		return Reply{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Kerge")
	resp, err := s.client.Do(req)
	if err != nil {
		return Reply{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxReplyBytes))
	if err != nil {
		return Reply{}, err
	}
	return Reply{Code: resp.StatusCode, Headers: headerLines(resp.Header), Body: readable(body)}, nil
}

// Reason describes why a request got no reply, without the address, which
// may hold a token.
func Reason(err error) (reason string, timedOut bool) {
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return "", true
		}
		err = ue.Err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "", true
	}
	msg := err.Error()
	// Errors from the network stack nest as "dial tcp …: connect: …";
	// the last part says what went wrong.
	if i := strings.LastIndex(msg, ": "); i >= 0 {
		msg = msg[i+2:]
	}
	return msg, false
}

func headerLines(h http.Header) string {
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		for _, v := range h[name] {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(name)
			b.WriteString(": ")
			b.WriteString(v)
		}
	}
	return b.String()
}

// readable indents a JSON body and makes any other body valid text.
func readable(body []byte) string {
	if json.Valid(body) {
		var b bytes.Buffer
		if json.Indent(&b, body, "", "  ") == nil {
			return b.String()
		}
	}
	if utf8.Valid(body) {
		return string(body)
	}
	return strings.ToValidUTF8(string(body), "�")
}
