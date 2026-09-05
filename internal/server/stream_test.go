package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCapturedTextFixture(t *testing.T) {
	f, err := os.Open(filepath.Join("..", "..", "test", "fixtures", "text.sse"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ch := parseStream(f)
	chunks, done := 0, false
	for !done {
		item, err := nextStreamItem(context.Background(), ch, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if item.done {
			done = true
		} else {
			chunks++
		}
	}
	if chunks != 3 {
		t.Fatalf("got %d chunks", chunks)
	}
}
