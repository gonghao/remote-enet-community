package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"remote-enet/internal/netpriv"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := netpriv.RunHelper(ctx, os.Args[1:]); err != nil {
		slog.Error("Network helper failed")
		os.Exit(1)
	}
}
