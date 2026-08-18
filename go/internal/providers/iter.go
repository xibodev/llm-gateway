package providers

import "errors"

// asError is a small wrapper around errors.As for the generic helpers above.
func asError[T error](err error, target *T) bool {
	return errors.As(err, target)
}

// sliceIter is a StreamIter backed by a pre-computed slice of chunks (used by
// the echo stub and any provider that buffers).
type sliceIter struct {
	chunks []string
	i      int
}

func (s *sliceIter) Next() (string, bool) {
	if s.i >= len(s.chunks) {
		return "", false
	}
	c := s.chunks[s.i]
	s.i++
	return c, true
}
func (s *sliceIter) Err() error   { return nil }
func (s *sliceIter) Close() error { return nil }
