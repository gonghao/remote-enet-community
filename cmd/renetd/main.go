// Command renetd runs the headless client and operator applications.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		slog.Error("renetd failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: renetd invite|operator|client [options]; use --help")
	}
	if args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		_, err := fmt.Fprintln(stdout, "Usage: renetd invite|operator|client [options]\nUse renetd <command> --help for command options.")
		return err
	}
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	var err error
	switch args[0] {
	case "invite":
		err = runInvite(ctx, args[1:], stdout, stderr)
	case "operator":
		err = runOperator(ctx, args[1:], stderr, logger)
	case "client":
		err = runClient(ctx, args[1:], stderr, logger)
	default:
		return fmt.Errorf("unknown command %q; use renetd --help", args[0])
	}
	if errors.Is(err, flag.ErrHelp) || (ctx.Err() != nil && errors.Is(err, ctx.Err())) {
		return nil
	}
	return err
}
