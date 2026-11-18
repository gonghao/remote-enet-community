package desktopui

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"remote-enet/internal/desktop"
	"remote-enet/internal/locales"
	"remote-enet/internal/netpriv"
	"remote-enet/internal/settings"
	renetui "remote-enet/internal/ui"
)

// Run is called by a role-specific executable on its main thread. The role is
// selected by that entry point and is never read from the command line.
func Run(role, version string, args []string) int {
	product, err := productForRole(role)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	opts, err := parseOptions(role, args, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err == nil && opts.version {
		fmt.Fprintf(os.Stdout, "%s %s\n", product.name, version)
		return 0
	}
	mygo.App.SetName(product.name)
	mygo.App.SetLanguages("en", "zh-Hans", "zh-Hant")
	if err == nil {
		mygo.App.SetPath(mygo.PathUserData, filepath.Join(filepath.Dir(opts.configPath), filepath.Base(opts.configPath)+".ui"))
	}
	mygo.App.SetVersion(version)
	mygo.Theme.SetSource(mygo.ThemeLight)
	if err != nil {
		showStartupError(product.name, err)
		return 2
	}
	if !mygo.App.RequestSingleInstanceLock() {
		return 0
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	network := bundledNetworkOptions()
	permission := netpriv.Inspect(network)
	controllerOpts := desktop.Options{ConfigPath: opts.configPath, StateDir: opts.stateDir, FixedRole: role}
	controllerOpts.NetworkStatus = func() netpriv.Status { return netpriv.Inspect(network) }
	if role == settings.RoleOperator {
		controllerOpts.DirectFactory = networkFactory(network)
	}
	controller, err := desktop.New(ctx, controllerOpts)
	if err != nil {
		showStartupError(product.name, fmt.Errorf("load %s: %w", opts.configPath, err))
		return 1
	}
	s := newShell(ctx, cancel, controller, product)
	s.version, s.permission = version, permissionState(permission)
	mygo.App.WhenReady(s.ready)
	mygo.App.OnSecondInstance(func([]string, string) { s.show() })
	// AppKit reopens the running application without starting a second process.
	mygo.App.OnActivate(func(hasVisibleWindows bool) {
		if !hasVisibleWindows {
			s.show()
		}
	})
	code := 0
	if err := mygo.App.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "application:", err)
		code = 1
	}
	cancel()
	if err := controller.Close(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "close application:", err)
		code = 1
	}
	s.workers.Wait()
	return code
}

func showStartupError(name string, err error) {
	mygo.App.WhenReady(func() {
		tr := func(source string, args ...any) string { return locales.Text(mygo.App.Locale(), source, args...) }
		mygo.NewWindow(mygo.WindowOptions{
			Title: tr("%s — startup error", tr(name)), Width: 720, Height: 300,
			BackgroundColor: renetui.BackgroundColor,
			Content: ui.View(func(c *ui.Context) {
				c.SetTheme(renetui.Theme())
				ui.Column(c).Fill().Padding(24).Gap(16).Children(func() {
					ui.Text(c, tr("%s could not start", tr(name))).FontSize(22).Bold()
					ui.Text(c, err.Error())
					ui.Text(c, tr("Existing settings have been kept. Correct the file and start again.")).TextColor(c.Theme().TextMuted)
					ui.PrimaryButton(c, tr("Quit")).OnClick(mygo.App.Quit)
				})
			}),
		})
	})
	if runErr := mygo.App.Run(); runErr != nil {
		fmt.Fprintln(os.Stderr, "application:", runErr)
	}
}
