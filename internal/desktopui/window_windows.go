package desktopui

import (
	"syscall"
	"unsafe"

	"github.com/egoist/mygo"
)

const mainWindowStartsHidden = false

var dwmSetWindowAttribute = syscall.NewLazyDLL("dwmapi.dll").NewProc("DwmSetWindowAttribute")

func prepareMainWindow(window *mygo.Window) {
	const dwmwaWindowCornerPreference = 33
	preference := int32(1) // DWMWCP_DONOTROUND
	// Windows 10 rejects this attribute and already uses square corners.
	_, _, _ = dwmSetWindowAttribute.Call(window.NativeHandle(), dwmwaWindowCornerPreference, uintptr(unsafe.Pointer(&preference)), unsafe.Sizeof(preference))
}
