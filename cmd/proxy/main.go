package main

import (
	"fmt"
	"github.com/local/codex-deepseek-proxy/internal/manage"
	"os"
)

func main() {
	if err := manage.Run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
