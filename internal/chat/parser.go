package chat

import (
	"bufio"
	"errors"
	"io"
	"strings"
)

var ErrEventTooLarge = errors.New("SSE event exceeds 16 MiB")

type SSEParser struct{ r *bufio.Reader }

func NewSSEParser(r io.Reader) *SSEParser { return &SSEParser{r: bufio.NewReaderSize(r, 64<<10)} }

// Next returns the joined data fields for one complete SSE event.
func (p *SSEParser) Next() ([]byte, error) {
	var data []string
	size := 0
	for {
		line, err := p.r.ReadString('\n')
		if err != nil && len(line) == 0 {
			if err == io.EOF && len(data) > 0 {
				return []byte(strings.Join(data, "\n")), nil
			}
			return nil, err
		}
		line = strings.TrimSuffix(line, "\n")
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			if len(data) > 0 {
				return []byte(strings.Join(data, "\n")), nil
			}
			if err != nil {
				return nil, err
			}
			continue
		}
		if !strings.HasPrefix(line, ":") {
			field, value, found := strings.Cut(line, ":")
			if !found {
				field, value = line, ""
			}
			if strings.HasPrefix(value, " ") {
				value = value[1:]
			}
			if field == "data" {
				size += len(value)
				if size > 16<<20 {
					return nil, ErrEventTooLarge
				}
				data = append(data, value)
			}
		}
		if err != nil {
			if len(data) > 0 {
				return []byte(strings.Join(data, "\n")), nil
			}
			return nil, err
		}
	}
}
