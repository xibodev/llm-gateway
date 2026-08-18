package providers

import (
	"bufio"
	"io"
)

// newLineReader adapts an io.Reader into the pull-style iterator the translate
// package expects: func() (line string, ok bool). Lines keep no trailing \n.
func newLineReader(r io.Reader) func() (string, bool) {
	reader := bufio.NewReader(r)
	return func() (string, bool) {
		for {
			line, err := reader.ReadString('\n')
			if len(line) > 0 {
				text := line
				// strip trailing newline chars
				for len(text) > 0 && (text[len(text)-1] == '\n' || text[len(text)-1] == '\r') {
					text = text[:len(text)-1]
				}
				return text, true
			}
			if err != nil {
				return "", false
			}
		}
	}
}
