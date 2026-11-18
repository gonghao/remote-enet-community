package ui

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/egoist/mygo/ui"

	"remote-enet/internal/hsfzcapture"
	"remote-enet/internal/settings"
)

func (v *View) canStartCapture() bool {
	return v.role == settings.RoleOperator && v.actions.StartCapture != nil && v.state.Running && !v.state.Stopping && !v.stopping && !v.state.Capture.Active && !v.state.Capture.Stopping && !v.captureStarting && !v.captureStopping
}

func (v *View) canStopCapture() bool {
	return v.role == settings.RoleOperator && v.actions.StopCapture != nil && (v.state.Capture.Active || v.state.Capture.Stopping || v.captureStarting) && !v.captureStopping
}

func captureText(s string, limit int) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= limit {
			b.WriteString("…")
			break
		}
		if unicode.IsControl(r) {
			b.WriteByte(' ')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (v *View) captureFrames() []hsfzcapture.Frame {
	recent := v.state.Capture.Recent
	return recent[max(0, len(recent)-200):]
}

func (v *View) captureMatches(f hsfzcapture.Frame) bool {
	return (v.captureAfterMarker == nil || !f.At.Before(v.captureAfterMarker.At)) && (v.captureDirection == "" || v.captureDirection == "All directions" || string(f.Direction) == v.captureDirection) && (strings.TrimSpace(v.captureConnection) == "" || strconv.FormatUint(f.Connection, 10) == strings.TrimSpace(v.captureConnection))
}

func (v *View) capturePage(c *ui.Context) {
	s := v.state.Capture
	v.pageTitle(c, "HSFZ capture", "Off by default · local JSONL application bytes, not PCAP")
	ui.Row(c).FillWidth().Gap(8).Children(func() {
		if ui.PrimaryButton(c.Key("capture-start"), v.tr("Start capture")).Label(v.tr("Start HSFZ capture")).Grow(1).MinWidth(0).Disabled(!v.canStartCapture()).Clicked() {
			v.captureStarting = true
			v.actions.StartCapture()
		}
		if ui.Button(c.Key("capture-stop"), v.tr("Stop capture")).Label(v.tr("Stop HSFZ capture")).Grow(1).MinWidth(0).Disabled(!v.canStopCapture()).Clicked() {
			v.captureStopping = true
			v.actions.StopCapture()
		}
	})
	status := "Disabled"
	switch {
	case s.Stopping || v.captureStopping:
		status = "Stopping capture"
	case s.Active:
		status = "Recording"
	case v.captureStarting:
		status = "Starting capture"
	}
	ui.Text(c, v.tr(status)).Role(ui.RoleStatus).Bold()
	if v.actions.MarkCapture != nil {
		ui.TextInput(c.Key("capture-marker-label"), &v.captureMarkerLabel).Label(v.tr("Marker label")).Placeholder(v.tr("Optional marker label · leave empty for automatic numbering")).Disabled(!v.canMarkCapture()).FillWidth().MinWidth(0).Height(32)
		ui.Text(c, v.tr("Insert marker is always available above. Labels use one line, at most 256 UTF-8 bytes.")).FillWidth().MinWidth(0).FontSize(11)
	}
	ui.Text(c, v.tr("Records diagnostic payloads, possibly including VIN, only to your selected local file. Reconnect diagnostic tools: only new TCP connections are captured.")).FillWidth().MinWidth(0).FontSize(11)
	path := captureText(s.Path, 1024)
	ui.TextInput(c.Key("capture-path"), &path).Label(v.tr("Capture file path")).ReadOnly(true).FillWidth().MinWidth(0).Height(32).Placeholder(v.tr("No capture file selected"))
	ui.Text(c, v.tr("Written %d B · observed %d B · recorded %d B\nFrames %d · dropped %d · active connections %d · incomplete %s", s.BytesWritten, s.ObservedBytes, s.RecordedBytes, s.Frames, s.DroppedEvents, s.ActiveConnections, v.tr(strconv.FormatBool(s.Incomplete)))).FillWidth().MinWidth(0).FontSize(11)
	if s.Error != "" {
		ui.Text(c, v.tr("Capture error / limit: %s", captureText(s.Error, 128))).FillWidth().MinWidth(0).TextColor(amber).Role(ui.RoleAlert)
	}
	v.messages(c)
	if len(s.RecentMarkers) > 0 {
		ui.Text(c, v.tr("Markers %d · recent boundaries", s.Markers)).FontSize(12).Bold()
		for i := len(s.RecentMarkers) - 1; i >= 0; i-- {
			m := s.RecentMarkers[i]
			ui.Text(c.Key(fmt.Sprintf("marker-label-%d", m.Sequence)), v.tr("#%d · %s · gen %d · %s", m.Sequence, m.At.Local().Format("15:04:05.000"), m.Generation, captureText(m.Label, 256))).FillWidth().MinWidth(0).FontSize(11)
			if ui.Button(c.Key(fmt.Sprintf("marker-%d", m.Sequence)), v.tr("Frames after marker %d", m.Sequence)).Label(v.tr("Show frames after marker %d", m.Sequence)).Clicked() {
				v.captureAfterMarker = &m
				v.captureHasSelection = false
			}
		}
	}
	if m := v.captureAfterMarker; m != nil {
		ui.Text(c, v.tr("Frames starting at or after marker %d · %s", m.Sequence, captureText(m.Label, 256))).FillWidth().MinWidth(0).FontSize(11)
		if ui.Button(c.Key("clear-marker-filter"), v.tr("Show all frame times")).Label(v.tr("Clear marker time filter")).Clicked() {
			v.captureAfterMarker = nil
		}
	}
	directions := []string{"All directions", string(hsfzcapture.ToVehicle), string(hsfzcapture.FromVehicle), string(hsfzcapture.ToOperator), string(hsfzcapture.FromOperator)}
	direction := v.tr(v.captureDirection)
	if v.captureDirection == "" {
		direction = v.tr("All directions")
	}
	translatedDirections := make([]string, len(directions))
	for i, d := range directions {
		translatedDirections[i] = v.tr(d)
	}
	ui.Select(c.Key("capture-direction"), &direction, translatedDirections).Label(v.tr("Capture direction filter")).FillWidth().MinWidth(0)
	for i, d := range translatedDirections {
		if direction == d {
			v.captureDirection = directions[i]
		}
	}
	ui.TextInput(c.Key("capture-connection"), &v.captureConnection).Label(v.tr("Capture connection filter")).Placeholder(v.tr("All connections · enter exact connection ID")).FillWidth().MinWidth(0).Height(32)
	frames := v.captureFrames()
	var selected *hsfzcapture.Frame
	for i := range frames {
		f := &frames[i]
		if v.captureMatches(*f) && v.captureHasSelection && f.Sequence == v.captureSelected {
			selected = f
		}
	}
	v.card(c.Key("capture-detail"), "Selected frame prefix", "Raw HSFZ header + body prefix only", func() {
		if selected == nil {
			ui.Text(c, v.tr("Select a recent frame to inspect its prefix.")).FontSize(11)
			return
		}
		f := *selected
		ui.Text(c, v.tr("Control 0x%04X · bodyLength %d · offset %d", f.Control, f.BodyLength, f.Offset)).FillWidth().MinWidth(0).FontSize(11)
		addresses := "Source / destination: unavailable"
		if f.HasAddresses {
			addresses = v.tr("Source 0x%02X · destination 0x%02X", f.Source, f.Destination)
		}
		ui.Text(c, v.tr(addresses)).FontSize(11)
		ui.Text(c, v.tr("Frame status: %s", v.tr(captureText(f.Status, 64)))).FontSize(11).TextColor(amber)
		shown := captureText(f.PrefixHex, 512)
		if len(f.PrefixHex) > 512 {
			shown = captureText(f.PrefixHex[:512], 512)
		}
		if f.PrefixTruncated || len(f.PrefixHex) > 512 {
			ui.Text(c, v.tr("Prefix truncated · this is not a complete large frame.")).FillWidth().MinWidth(0).FontSize(11).TextColor(amber)
		}
		if ui.Button(c.Key("capture-copy"), v.tr("Copy shown hex")).Label(v.tr("Copy shown hex")).Disabled(v.actions.Copy == nil || shown == "").Clicked() {
			v.actions.Copy(shown)
		}
		// Short lines keep the prefix readable at small widths without expanding layout.
		for i := 0; i < len(shown); i += 32 {
			ui.Text(c.Key(i), shown[i:min(i+32, len(shown))]).FontSize(11).FillWidth().MinWidth(0)
		}
	})
	ui.Text(c, v.tr("Recent frames · at most 200 · newest first")).FontSize(12).Bold()
	count := 0
	for i := len(frames) - 1; i >= 0; i-- {
		f := frames[i]
		if !v.captureMatches(f) {
			continue
		}
		count++
		label := v.tr("Frame %d · %s · conn %d · gen %d · %s · control 0x%04X · body %d · %s", f.Sequence, f.At.Local().Format("15:04:05.000"), f.Connection, f.Generation, v.tr(captureText(string(f.Direction), 32)), f.Control, f.BodyLength, v.tr(captureText(f.Status, 32)))
		ui.Column(c.Key(fmt.Sprintf("capture-row-%d", f.Sequence))).FillWidth().MinWidth(0).Gap(4).Children(func() {
			if ui.Button(c.Key("select"), v.tr("Frame %d", f.Sequence)).Label(v.tr("Select capture frame %d", f.Sequence)).FillWidth().MinWidth(0).Height(32).Clicked() {
				v.captureSelected, v.captureHasSelection = f.Sequence, true
			}
			ui.Text(c, v.tr(label)).FillWidth().MinWidth(0).FontSize(11)
		})
	}
	if count == 0 {
		ui.Text(c, v.tr("No matching recent frames.")).TextColor(muted)
	}
}
