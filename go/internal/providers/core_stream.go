package providers

import (
	"bytes"
	"errors"
	"io"

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
// began with.
type relayedStream struct {
	inner  core.StreamIter
	prefix string
	err    error
}

func (s *relayedStream) Next() (string, bool) {
	data, err := nextCoreData(s.inner)
	if err != nil {
		s.err = relayedStreamEnd(err, s.prefix)
		return "", false
	}
	return data, true
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
