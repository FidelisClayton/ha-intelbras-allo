package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/FidelisClayton/ha-intelbras-allo/internal/runtime"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if runtime.Run(ctx, os.Stdin, os.Stdout) != nil {
		os.Exit(1)
	}
}
