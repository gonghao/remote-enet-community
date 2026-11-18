package desktopui

import (
	"strconv"
	"strings"

	"github.com/egoist/mygo"
	_ "github.com/egoist/mygo/locales/zh-Hans"
	_ "github.com/egoist/mygo/locales/zh-Hant"

	"remote-enet/internal/locales"
)

func (s *shell) tr(source string, args ...any) string {
	return locales.Text(s.language, source, args...)
}

func (s *shell) displayMessage(message string) string {
	if value, ok := strings.CutPrefix(message, "Marker "); ok {
		if number, ok := strings.CutSuffix(value, " queued."); ok {
			if n, err := strconv.ParseUint(number, 10, 64); err == nil {
				return s.tr("Marker %d queued.", n)
			}
		}
	}
	return s.tr(message)
}

func (s *shell) setLanguage(tag string) {
	if s.quitting {
		return
	}
	switch tag {
	case "", "en", "zh-Hans", "zh-Hant":
	default:
		return
	}
	mygo.App.SetLocale(tag)
	// Following the system and explicitly choosing its current language have
	// the same effective locale, so MyGo emits no LocaleChanged notification.
	if s.chosenLanguage != mygo.App.ChosenLocale() {
		s.localeChanged(mygo.App.Locale())
	}
}

func (s *shell) localeChanged(tag string) {
	if s.quitting {
		return
	}
	s.language = locales.Match(tag)
	s.chosenLanguage = mygo.App.ChosenLocale()
	if s.window != nil {
		s.window.SetTitle(s.tr(s.product.name))
	}
	if s.tray != nil {
		s.tray.SetMenu(s.trayMenu())
	}
	s.applyState()
	s.invalidate()
}
