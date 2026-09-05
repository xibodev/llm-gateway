package providers

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestSSERecordReaderParsesLogicalEvents(t *testing.T) {
	stream := strings.Join([]string{
		": comment\r",
		"event: message\r",
		"id: 1\r",
		"retry: 100\r",
		"unknown: ignored\r",
		"data:first\r",
		"data: second\r",
		"data:  two spaces remain\r",
		"\r",
		"data:\r",
		"\r",
		"event: no-data\r",
		"\r",
		"data: [DONE]\r",
		"\r",
		"data: final",
	}, "\n")
	reader := newSSERecordReader(strings.NewReader(stream))

	if got, ok := reader.Next(); !ok || got != "first\nsecond\n two spaces remain" {
		t.Fatalf("first event = %q, %v", got, ok)
	}
	if got, ok := reader.Next(); !ok || got != "final" {
		t.Fatalf("EOF event = %q, %v", got, ok)
	}
	if got, ok := reader.Next(); ok || got != "" || reader.Err() != nil {
		t.Fatalf("end = %q, %v, err %v", got, ok, reader.Err())
	}
}

func TestSSERecordReaderWireLimit(t *testing.T) {
	for _, tc := range []struct {
		name      string
		extraByte int
		wantOK    bool
	}{
		{name: "exact limit", wantOK: true},
		{name: "limit plus one", extraByte: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := strings.Repeat("x", maxStreamRecordWireSize-len("data: \n\n")+tc.extraByte)
			reader := newSSERecordReader(strings.NewReader("data: " + payload + "\n\n"))
			got, ok := reader.Next()
			if ok != tc.wantOK {
				t.Fatalf("Next ok = %v, want %v", ok, tc.wantOK)
			}
			if tc.wantOK && got != payload {
				t.Fatalf("payload length = %d, want %d", len(got), len(payload))
			}
			if !tc.wantOK {
				var sizeErr *StreamRecordTooLargeError
				if !errors.As(reader.Err(), &sizeErr) || sizeErr.Format != "SSE" || sizeErr.Limit != maxStreamRecordWireSize {
					t.Fatalf("error = %#v", reader.Err())
				}
				if strings.Contains(sizeErr.Error(), payload[:32]) {
					t.Fatalf("error exposes payload: %q", sizeErr)
				}
			}
		})
	}
}

func TestSSERecordReaderCountsIgnoredWireLines(t *testing.T) {
	reader := newSSERecordReader(io.MultiReader(
		strings.NewReader(": "+strings.Repeat("x", maxStreamRecordWireSize-3)+"\n"),
		strings.NewReader("data: exposed\n\n"),
	))
	if _, ok := reader.Next(); ok {
		t.Fatal("oversized ignored line was accepted")
	}
	var sizeErr *StreamRecordTooLargeError
	if !errors.As(reader.Err(), &sizeErr) {
		t.Fatalf("error = %v", reader.Err())
	}
}

func TestHTTPStreamIterReturnsCompleteRecordsAndParserErrors(t *testing.T) {
	response := &http.Response{Body: io.NopCloser(strings.NewReader(
		"data: one\ndata: two\n\ndata: " + strings.Repeat("x", maxStreamRecordWireSize) + "\n\n",
	))}
	iter := newHTTPStreamIter(response, "test")
	defer iter.Close()

	if got, ok := iter.Next(); !ok || got != "one\ntwo" {
		t.Fatalf("record = %q, %v", got, ok)
	}
	if _, ok := iter.Next(); ok {
		t.Fatal("oversized record was accepted")
	}
	var sizeErr *StreamRecordTooLargeError
	if !errors.As(iter.Err(), &sizeErr) {
		t.Fatalf("error = %#v", iter.Err())
	}
}
