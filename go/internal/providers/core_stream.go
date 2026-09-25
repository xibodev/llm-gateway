package providers

import (
	"bytes"

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
