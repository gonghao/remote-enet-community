package main

import (
	"context"
	"io"
	"testing"
	"time"
)

func TestInvalidOptions(t *testing.T) {
	for _, args := range [][]string{{"--profile", "unknown"}, {"--bind", "bad"}, {"--vin", "short"}, {"--tester", "256"}, {"--mac", "bad"}, {"--profile", "drop-alive"}, {"--profile", "long-alive"}, {"--alive", "1s"}, {"--short-alive-body", "00"}, {"--kl15", "zz"}, {"--unknown-nrc", "00"}, {"--diag-port", "65536"}, {"--responders", "/nonexistent/fakezgw.json"}, {"unexpected"}} {
		t.Run(args[0]+args[len(args)-1], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			args = append(args, "--diag-port", "0", "--ctrl-port", "0", "--disc-port", "0")
			if args[0] == "--diag-port" {
				args = []string{"--diag-port", "65536"}
			}
			if err := run(ctx, args, io.Discard); err == nil {
				t.Fatal("invalid flags accepted")
			}
		})
	}
}
func TestProfiles(t *testing.T) {
	for _, name := range []string{"normal", "no-alive", "drop-alive", "long-alive", "error-frame"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
			defer cancel()
			if err := run(ctx, []string{"--profile", name, "--alive", "10ms", "--alive-timeout", "20ms", "--short-alive-body", "1234", "--kl15", "04", "--diag-port", "0", "--ctrl-port", "0", "--disc-port", "0"}, io.Discard); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAliveResponseControlFlag(t *testing.T) {
	for _, value := range []string{"0x12", "0x13", "0", "0x14", "65555"} {
		t.Run(value, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
			defer cancel()
			err := run(ctx, []string{"--alive-response-control", value, "--diag-port", "0", "--ctrl-port", "0", "--disc-port", "0"}, io.Discard)
			valid := value == "0x12" || value == "0x13"
			if (err == nil) != valid {
				t.Fatalf("control %s: %v", value, err)
			}
		})
	}
}
