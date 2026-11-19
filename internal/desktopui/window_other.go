//go:build !darwin && !windows

package desktopui

import "github.com/egoist/mygo"

const mainWindowStartsHidden = false

func prepareMainWindow(*mygo.Window) {}
