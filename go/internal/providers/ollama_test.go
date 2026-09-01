package providers

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOllamaStreamNormalAndOversizedRecords(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response string
		wantText string
		wantErr  bool
	}{
		{name: "normal", response: `{"message":{"content":"hello"},"done":false}` + "\n" + `{"done":true}` + "\n", wantText: "hello"},
		{name: "oversized", response: strings.Repeat("x", maxStreamRecordWireSize+1) + "\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprint(w, tc.response)
			}))
			defer server.Close()

			iter, err := (OllamaProvider{BaseURL: server.URL, Timeout: 2}).Stream("model", []Message{{"role": "user", "content": "hi"}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer iter.Close()
			var chunks strings.Builder
			for chunk, ok := iter.Next(); ok; chunk, ok = iter.Next() {
				chunks.WriteString(chunk)
			}
			if tc.wantText != "" && !strings.Contains(chunks.String(), tc.wantText) {
				t.Fatalf("chunks = %s", chunks.String())
			}
			var sizeErr *StreamRecordTooLargeError
			if errors.As(iter.Err(), &sizeErr) != tc.wantErr {
				t.Fatalf("error = %#v, want oversized %v", iter.Err(), tc.wantErr)
			}
			if sizeErr != nil && (sizeErr.Format != "NDJSON" || strings.Contains(sizeErr.Error(), strings.Repeat("x", 32))) {
				t.Fatalf("unsafe error = %#v", sizeErr)
			}
		})
	}
}
