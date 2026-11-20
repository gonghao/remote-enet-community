package ui

import (
	"slices"
	"strings"

	"github.com/egoist/mygo/i18n"
	"github.com/egoist/mygo/ui"

	"remote-enet/internal/locales"
)

// SetLanguage changes display text without replacing forms or request latches.
func (v *View) SetLanguage(tag string) { v.language = locales.Match(tag) }

func (v *View) tr(source string, args ...any) string {
	return locales.Text(v.language, source, args...)
}

func (v *View) validationMessage(err error) string {
	message := err.Error()
	switch {
	case strings.HasPrefix(message, "settings: center."):
		return v.tr("Check the center ID and HTTPS addresses.")
	case strings.HasPrefix(message, "settings: operator.hub.addr:"):
		return v.tr("Hub address must include a host and port.")
	case strings.HasPrefix(message, "settings: operator.hub.fp"):
		return v.tr("Enter the full Hub fingerprint.")
	case strings.HasPrefix(message, "settings: operator.listen:"):
		return v.tr("Listen address must be a valid IP or hostname followed by a port.")
	case strings.HasPrefix(message, "settings: operator.endpoint:"):
		return v.tr("Public endpoint must be a valid IP or hostname followed by a port.")
	case strings.HasPrefix(message, "settings: operator.bind"):
		return v.tr("Presentation IPv4 must be a literal unicast address.")
	case strings.HasPrefix(message, "settings: operator diagnostic and control TCP ports"):
		return v.tr("Diagnostic and control TCP ports must be different.")
	default:
		return v.tr(message)
	}
}

func (v *View) languageSelect(c *ui.Context) {
	tags := []string{"", "zh-Hans", "zh-Hant", "en"}
	labels := []string{v.tr("Follow system"), i18n.Autonym("zh-Hans"), i18n.Autonym("zh-Hant"), i18n.Autonym("en")}
	selected := 0
	for i, tag := range tags {
		if v.state.ChosenLanguage != "" && tag == locales.Match(v.state.ChosenLanguage) {
			selected = i
		}
	}
	ui.Field(c.Key("language-field"), v.tr("Interface language"), func() {
		choice := labels[selected]
		if ui.Select(c.Key("language"), &choice, labels).Label(v.tr("Interface language")).Disabled(v.actions.SetLanguage == nil).FillWidth().MinWidth(0).Height(32).Changed() {
			v.actions.SetLanguage(tags[slices.Index(labels, choice)])
		}
	}).FillWidth()
	ui.Text(c, v.tr("Changes apply immediately. Network settings and unsaved edits are kept.")).FontSize(11).FillWidth().TextColor(muted)
}
