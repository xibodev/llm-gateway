package providers

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"

	core "github.com/xibodev/llmgw-core"
)

// nextCoreData returns the data of the next record a core stream relays, as
// the transport's reader returned the upstream's records. Core relays each
// record byte for byte, so the transport's reader parses it as it parsed the
// upstream, and skips one of [DONE] or without data. The stream's error,
// io.EOF at its end, is returned as it is, for the facade to read.
func nextCoreData(stream core.StreamIter) (string, error) {
	for {
		frame, err := stream.Next()
		if err != nil {
			return "", err
		}
		if data, ok := newSSERecordReader(bytes.NewReader(frame)).Next(); ok {
			return data, nil
		}
	}
}

// relayedStream relays a core stream of SSE records as the data events the
// API layer reads, as the gateway's HTTP stream returned them; see
// relayedStreamEnd for how it ends. prefix is what that stream's errors
// began with. responses marks a stream of Responses events, which ends with
// a terminal event the API layer checks itself.
//
// A Chat stream is complete once the upstream sent [DONE] or a chunk with a
// finish reason. Core relays the end of the body as it is, so an upstream
// that closes the stream before either, a proxy timing out the connection
// cleanly included, cut the answer short and fails it.
type relayedStream struct {
	inner     core.StreamIter
	prefix    string
	responses bool
	finished  bool
	err       error
}

func (s *relayedStream) Next() (string, bool) {
	for {
		frame, err := s.inner.Next()
		if err != nil {
			s.err = relayedStreamEnd(err, s.prefix)
			if s.err == nil && !s.responses && !s.finished {
				s.err = circuitFailureInvocation(s.prefix + ": stream ended without a finish reason")
			}
			return "", false
		}
		data, ok := newSSERecordReader(bytes.NewReader(frame)).Next()
		if !ok {
			// The reader skips [DONE] as it skips a record without data.
			s.finished = s.finished || relayedDone(frame)
			continue
		}
		s.finished = s.finished || chatChunkFinishes(data)
		return data, true
	}
}

func (s *relayedStream) Err() error   { return s.err }
func (s *relayedStream) Close() error { return s.inner.Close() }

// relayedStreamEnd is how a relayed stream ends for what core's stream
// returned, as the gateway's HTTP stream ended: without an error at the
// stream's end, with the gateway's StreamRecordTooLargeError for a record
// over the size limit, the one upstream failure a relayed stream reports,
// and with the streaming error prefix names when the stream broke.
func relayedStreamEnd(err error, prefix string) error {
	var failure *core.ProviderError
	switch {
	case errors.Is(err, io.EOF):
		return nil
	case !errors.As(err, &failure):
		return err
	case failure.Class == core.ProviderErrorUpstream:
		return &StreamRecordTooLargeError{Format: "SSE", Limit: maxStreamRecordWireSize}
	case failure.Cause != nil:
		return &InvocationError{Msg: prefix + ": streaming transport error: " + failure.Cause.Error()}
	}
	return err
}

// relayedDone reports a record whose data is [DONE], read as the gateway's
// reader reads a record's data lines.
func relayedDone(frame []byte) bool {
	var data []string
	for _, line := range strings.Split(string(frame), "\n") {
		if field, value, _ := strings.Cut(strings.TrimSuffix(line, "\r"), ":"); field == "data" {
			data = append(data, strings.TrimPrefix(value, " "))
		}
	}
	return strings.Join(data, "\n") == "[DONE]"
}

// chatChunkFinishes reports a Chat chunk with a choice that has finished.
func chatChunkFinishes(data string) bool {
	var chunk struct {
		Choices []struct {
			FinishReason any `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal([]byte(data), &chunk) != nil {
		return false
	}
	for _, choice := range chunk.Choices {
		if reason, _ := choice.FinishReason.(string); reason != "" {
			return true
		}
	}
	return false
}
