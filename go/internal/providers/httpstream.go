package providers

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// httpStreamIter reads an SSE response line-by-line, stripping "data:" prefixes
// and skipping blanks / [DONE]. Mid-stream transport errors surface via Err.
type httpStreamIter struct {
	resp   *http.Response
	reader *bufio.Reader
	err    error
	prefix string // error message prefix for this provider
}

func newHTTPStreamIter(resp *http.Response, prefix string) *httpStreamIter {
	return &httpStreamIter{resp: resp, reader: bufio.NewReader(resp.Body), prefix: prefix}
}

func (it *httpStreamIter) Next() (string, bool) {
	for {
		line, err := it.reader.ReadString('\n')
		if len(line) > 0 {
			text := strings.TrimRight(line, "\r\n")
			if text == "" {
				if err != nil {
					it.finish(err)
					return "", false
				}
				continue
			}
			if strings.HasPrefix(text, "data:") {
				text = strings.TrimSpace(text[len("data:"):])
			}
			if text == "" || text == "[DONE]" {
				if err != nil {
					it.finish(err)
					return "", false
				}
				continue
			}
			return text, true
		}
		if err != nil {
			it.finish(err)
			return "", false
		}
	}
}

func (it *httpStreamIter) finish(err error) {
	if err != nil && err != io.EOF {
		it.err = &InvocationError{Msg: it.prefix + ": streaming transport error: " + err.Error()}
	}
}

func (it *httpStreamIter) Err() error { return it.err }
func (it *httpStreamIter) Close() error {
	if it.resp != nil {
		return it.resp.Body.Close()
	}
	return nil
}

// ---- shared HTTP client ------------------------------------------------- //

func httpClient(timeout float64) *http.Client {
	return &http.Client{Timeout: time.Duration(timeout * float64(time.Second))}
}

func decodeJSON(r io.Reader) (map[string]any, error) {
	var out map[string]any
	dec := json.NewDecoder(r)
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// extractError pulls a human error message out of a JSON error body, falling
// back to the raw text.
func extractError(body []byte) string {
	var obj map[string]any
	if json.Unmarshal(body, &obj) == nil {
		if err, ok := obj["error"].(map[string]any); ok {
			if msg, ok := err["message"].(string); ok && msg != "" {
				return msg
			}
		}
		if s, ok := obj["error"].(string); ok {
			return s
		}
		if len(body) > 512 {
			return string(body[:512])
		}
		return string(body)
	}
	t := strings.TrimSpace(string(body))
	if t == "" {
		return "no body"
	}
	return t
}
