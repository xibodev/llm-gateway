package providers

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

const maxStreamRecordWireSize = 4 << 20

// StreamRecordTooLargeError safely describes an oversized upstream stream
// record without retaining or exposing any of its payload.
type StreamRecordTooLargeError struct {
	Format string
	Limit  int
}

func (e *StreamRecordTooLargeError) Error() string {
	return fmt.Sprintf("upstream %s record exceeds %d-byte wire limit", e.Format, e.Limit)
}

func readBoundedLine(reader *bufio.Reader, limit int, format string) ([]byte, error) {
	var line []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(fragment) > limit-len(line) {
			return nil, &StreamRecordTooLargeError{Format: format, Limit: maxStreamRecordWireSize}
		}
		line = append(line, fragment...)
		if err != bufio.ErrBufferFull {
			return line, err
		}
	}
}

type sseRecordReader struct {
	reader *bufio.Reader
	err    error
}

func newSSERecordReader(r io.Reader) *sseRecordReader {
	return &sseRecordReader{reader: bufio.NewReader(r)}
}

// Next returns one complete SSE event data payload. The limit applies to all
// wire bytes in an event, including ignored fields and its blank delimiter.
func (r *sseRecordReader) Next() (string, bool) {
	var data []string
	wireSize := 0
	for {
		line, err := readBoundedLine(r.reader, maxStreamRecordWireSize-wireSize, "SSE")
		wireSize += len(line)
		if err != nil && err != io.EOF {
			r.err = err
			return "", false
		}

		if len(line) > 0 {
			line = trimSSELineEnding(line)
			if len(line) == 0 {
				if payload := strings.Join(data, "\n"); payload != "" && payload != "[DONE]" {
					return payload, true
				}
				data = nil
				wireSize = 0
			} else if line[0] != ':' {
				field, value, found := strings.Cut(string(line), ":")
				if !found {
					value = ""
				}
				if len(value) > 0 && value[0] == ' ' {
					value = value[1:]
				}
				if field == "data" {
					data = append(data, value)
				}
			}
		}

		if err == io.EOF {
			if payload := strings.Join(data, "\n"); payload != "" && payload != "[DONE]" {
				return payload, true
			}
			return "", false
		}
	}
}

func (r *sseRecordReader) Err() error { return r.err }

func trimSSELineEnding(line []byte) []byte {
	if len(line) == 0 || line[len(line)-1] != '\n' {
		return line
	}
	line = line[:len(line)-1]
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	return line
}
