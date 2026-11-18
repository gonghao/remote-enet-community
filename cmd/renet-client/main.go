package main

import (
	"os"

	"remote-enet/internal/desktopui"
	"remote-enet/internal/settings"
)

var version = "dev"

func main() {
	os.Exit(desktopui.Run(settings.RoleClient, version, os.Args[1:]))
}
