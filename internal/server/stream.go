package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/local/codex-deepseek-proxy/internal/chat"
)

type streamItem struct {
	chunk chat.CompletionChunk
	done  bool
	err   error
}

func parseStream(r io.Reader) <-chan streamItem {
	out := make(chan streamItem, 1)
	go func() {
		defer close(out)
		p := chat.NewSSEParser(r)
		for {
			data, err := p.Next()
			if err != nil {
				out <- streamItem{err: err}
				return
			}
			if string(data) == "[DONE]" {
				out <- streamItem{done: true}
				return
			}
			var c chat.CompletionChunk
			if err := json.Unmarshal(data, &c); err != nil {
				out <- streamItem{err: fmt.Errorf("invalid upstream SSE JSON: %w", err)}
				return
			}
			if c.Object != "" && c.Object != "chat.completion.chunk" {
				out <- streamItem{err: fmt.Errorf("unexpected upstream object %q", c.Object)}
				return
			}
			out <- streamItem{chunk: c}
		}
	}()
	return out
}

func nextStreamItem(ctx context.Context, ch <-chan streamItem, idle time.Duration) (streamItem, error) {
	t := time.NewTimer(idle)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return streamItem{}, ctx.Err()
	case <-t.C:
		return streamItem{}, errors.New("upstream stream idle timeout")
	case item, ok := <-ch:
		if !ok {
			return streamItem{}, io.EOF
		}
		if item.err != nil {
			return streamItem{}, item.err
		}
		return item, nil
	}
}
