package desktopui

import (
	"flag"
	"fmt"
	"io"
	"path/filepath"

	"remote-enet/internal/settings"
)

type options struct {
	configPath string
	stateDir   string
	version    bool
}

func parseOptions(role string, args []string, out io.Writer) (options, error) {
	return parseOptionsWithPaths(role, args, out, settings.DefaultPathsForRole)
}

func parseOptionsWithPaths(role string, args []string, out io.Writer, paths func(string) (string, string, error)) (options, error) {
	product, err := productForRole(role)
	if err != nil {
		return options{}, err
	}
	fs := flag.NewFlagSet(product.command, flag.ContinueOnError)
	fs.SetOutput(out)
	var opts options
	fs.StringVar(&opts.configPath, "config", "", "Settings JSON path (defaults to this role's user profile)")
	fs.StringVar(&opts.stateDir, "state-dir", "", "Identity and trust directory (defaults to the user profile)")
	fs.BoolVar(&opts.version, "version", false, "Print application version")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if fs.NArg() != 0 {
		return options{}, fmt.Errorf("unexpected positional arguments")
	}
	explicitEmpty := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "config" && opts.configPath == "" || f.Name == "state-dir" && opts.stateDir == "" {
			explicitEmpty = true
		}
	})
	if explicitEmpty {
		return options{}, fmt.Errorf("config and state-dir must be nonempty")
	}
	if opts.version {
		return opts, nil
	}
	if opts.configPath == "" || opts.stateDir == "" {
		configPath, stateDir, err := paths(role)
		if err != nil {
			return options{}, fmt.Errorf("locate application data: %w", err)
		}
		if opts.configPath == "" {
			opts.configPath = configPath
		}
		if opts.stateDir == "" {
			opts.stateDir = stateDir
		}
	}
	opts.configPath, err = filepath.Abs(opts.configPath)
	if err != nil {
		return options{}, fmt.Errorf("resolve settings path: %w", err)
	}
	opts.stateDir, err = filepath.Abs(opts.stateDir)
	if err != nil {
		return options{}, fmt.Errorf("resolve state directory: %w", err)
	}
	return opts, nil
}
