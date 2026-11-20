package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"remote-enet/internal/app"
	"remote-enet/internal/settings"
)

func loadCenter(path, role string) (*settings.Center, error) {
	if path == "" {
		var err error
		path, _, err = settings.DefaultPathsForRole(role)
		if err != nil {
			return nil, err
		}
	}
	cfg, err := settings.Load(path)
	if err != nil {
		return nil, err
	}
	if cfg.Center.ID == "" {
		return nil, app.PairingErrorKind("center_unconfigured")
	}
	return &cfg.Center, nil
}
func wasSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// Read one bounded, terminated line. EOF alone must never confirm a client prompt.
func copyAnswer(in io.Reader, out io.Writer) error {
	var b [1]byte
	var line strings.Builder
	for line.Len() < 128 {
		if _, err := io.ReadFull(in, b[:]); err != nil {
			return err
		}
		if b[0] == '\n' {
			_, err := io.WriteString(out, strings.TrimSuffix(line.String(), "\r"))
			return err
		}
		line.WriteByte(b[0])
	}
	return errors.New("input exceeds limit")
}

func readAnswer(ctx context.Context) (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, path, "--read-center-answer")
	cmd.Stdin = os.Stdin
	b, err := cmd.Output()
	return string(b), err
}

func sasMatches(input, code string) bool {
	norm := strings.Map(func(r rune) rune {
		switch {
		case r == ' ' || r == '　':
			return -1
		case r >= '０' && r <= '９':
			return '0' + r - '０'
		default:
			return r
		}
	}, input)
	return len(code) == 6 && norm == code
}

func centerConsole(ctx context.Context, r *app.Runtime, updates <-chan app.Snapshot, out io.Writer) {
	var shown string
	var seen uint64
	var retry bool
	var stopInput func()
	stop := func() {
		if stopInput != nil {
			stopInput()
			stopInput = nil
		}
	}
	defer stop()
	for state := range updates {
		code, p := r.CenterDisplay()
		if code != "" && code != shown {
			fmt.Fprintf(out, "Center invitation: %s %s\n", code[:4], code[4:])
		}
		shown = code
		waitingRetry := state.Center.Phase == "retry"
		if seen == p.ID && retry == waitingRetry {
			continue
		}
		stop()
		seen, retry = p.ID, waitingRetry
		if seen == 0 && !retry {
			continue
		}
		inputCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		stopInput = func() { cancel(); <-done }
		go func() {
			defer close(done)
			if waitingRetry {
				fmt.Fprintln(out, "Pairing interrupted. Press Enter to retry or type no to end reception.")
				line, err := readAnswer(inputCtx)
				if inputCtx.Err() != nil {
					return
				}
				if err == nil && line == "" {
					_ = r.RetryPairing(inputCtx)
				} else {
					endCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
					defer cancel()
					_ = r.EndReception(endCtx)
				}
				return
			}
			promptCtx, end := context.WithDeadline(inputCtx, p.Deadline)
			defer end()
			match := confirmConsole(promptCtx, state.Role == "operator", p.Code, out)
			_ = r.AnswerSAS(promptCtx, p.ID, match)
		}()
	}
}

func confirmConsole(ctx context.Context, operator bool, code string, out io.Writer) bool {
	for ctx.Err() == nil {
		if operator {
			fmt.Fprintln(out, "Enter the six digits read by the customer, or type no to reject.")
		} else {
			fmt.Fprintf(out, "Read %s to the operator. Press Enter to confirm or type no to reject.\n", code)
		}
		input, err := readAnswer(ctx)
		if err != nil || strings.EqualFold(input, "no") {
			return false
		}
		if operator && sasMatches(input, code) || !operator && input == "" {
			return true
		}
	}
	return false
}
