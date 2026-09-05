package chat

import (
	"io"
	"strings"
	"testing"
)

func TestSSEParser(t *testing.T) {
	tests := []struct{ name, input, want string }{
		{"lf", "data: one\n\n", "one"},
		{"crlf", "data: two\r\n\r\n", "two"},
		{"multiline", "event: x\ndata: first\ndata: second\n\n", "first\nsecond"},
		{"comments and empty", ": ping\n\ndata: ok\n\n", "ok"},
		{"done", "data: [DONE]\n\n", "[DONE]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewSSEParser(strings.NewReader(tt.input))
			got, err := p.Next()
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

type oneByteReader struct {
	s string
	i int
}

func (r *oneByteReader) Read(p []byte) (int, error) {
	if r.i == len(r.s) {
		return 0, io.EOF
	}
	p[0] = r.s[r.i]
	r.i++
	return 1, nil
}

func TestSSEParserPartialReads(t *testing.T) {
	p := NewSSEParser(&oneByteReader{s: "data: partial\n\n"})
	got, err := p.Next()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "partial" {
		t.Fatalf("got %q", got)
	}
}

func TestSSEParserEOF(t *testing.T) {
	_, err := NewSSEParser(strings.NewReader("")).Next()
	if err != io.EOF {
		t.Fatalf("got %v", err)
	}
}
