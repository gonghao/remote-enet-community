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

var version = "dev"

func main() {
	// A separate reader lets prompt cancellation interrupt a console read on all platforms.
	if len(os.Args) == 2 && os.Args[1] == "--read-center-answer" {
		if err := copyAnswer(os.Stdin, os.Stdout); err != nil {
			os.Exit(1)
		}
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		slog.Error("renetd failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: renetd invite|issuer|operator|client [options]; use --help")
	}
	if args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		_, err := fmt.Fprintln(stdout, "Usage: renetd invite|issuer|operator|client [options]\nUse renetd <command> --help for command options.")
		return err
	}
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	var err error
	switch args[0] {
	case "issuer":
		err = runIssuer(ctx, args[1:], stdout, stderr)
	case "invite":
		err = runInvite(ctx, args[1:], stdout, stderr)
	case "operator":
		err = runOperator(ctx, args[1:], stdout, stderr, logger)
	case "client":
		err = runClient(ctx, args[1:], stdout, stderr, logger)
	default:
		return fmt.Errorf("unknown command %q; use renetd --help", args[0])
	}
	if errors.Is(err, flag.ErrHelp) || (ctx.Err() != nil && errors.Is(err, ctx.Err())) {
		return nil
	}
	return err
}
