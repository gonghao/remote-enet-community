package desktopui

import (
	"github.com/ebitengine/purego/objc"
	"github.com/egoist/mygo"
)

const mainWindowStartsHidden = true

// Called on the AppKit main thread from App.WhenReady.
func prepareMainWindow(window *mygo.Window) {
	const styleTitled = 1 << 0
	nsWindow := objc.ID(window.NativeHandle())
	style := uintptr(nsWindow.Send(objc.RegisterName("styleMask")))
	// MyGo's Frameless leaves NSWindowStyleMaskTitled set, so AppKit still
	// rounds the native frame and shadow even when the content is square.
	nsWindow.Send(objc.RegisterName("setStyleMask:"), style&^styleTitled)
	nsWindow.Send(objc.RegisterName("setHasShadow:"), true)
	nsWindow.Send(objc.RegisterName("invalidateShadow"))
	window.OnReadyToShow(window.Show)
}
