package brand

import (
	"bytes"
	"embed"
	"encoding/binary"
	"image/color"
	"image/png"
	"testing"
)

//go:embed assets/*
var exported embed.FS

func TestPixelGridAndTemplateCutouts(t *testing.T) {
	for _, size := range []int{16, 18, 20, 32, 36, 48, 128} {
		plain, err := png.Decode(bytes.NewReader(PNG(size, false, false)))
		if err != nil {
			t.Fatal(err)
		}
		mono, err := png.Decode(bytes.NewReader(PNG(size, false, true)))
		if err != nil {
			t.Fatal(err)
		}
		if plain.Bounds().Dx() != size || plain.Bounds().Dy() != size {
			t.Fatal("invalid dimensions")
		}
		for y := range size {
			for x := range size {
				pixel := color.NRGBAModel.Convert(plain.At(x, y)).(color.NRGBA)
				r, g, b, templateAlpha := mono.At(x, y).RGBA()
				if pixel.A != 0 && pixel.A != 255 {
					t.Fatal("pixel artwork has a semitransparent halo")
				}
				var expectedAlpha uint32
				switch pixel {
				case color.NRGBA{R: 35, G: 86, B: 216, A: 255}:
					expectedAlpha = 65535
				case color.NRGBA{R: 255, G: 255, B: 255, A: 255}, color.NRGBA{}:
				default:
					t.Fatalf("unexpected pixel at %dpx (%d,%d): %v", size, x, y, pixel)
				}
				if templateAlpha != expectedAlpha || r != 0 || g != 0 || b != 0 {
					t.Fatalf("template lost a cutout at %dpx (%d,%d)", size, x, y)
				}
			}
		}
		if !bytes.Equal(PNG(size, true, true), PNG(size, false, true)) {
			t.Fatal("template must not gain an opaque plate")
		}
	}
}

func TestCompactGridsAndRetinaMenu(t *testing.T) {
	for _, item := range []struct {
		size, row, groups int
	}{{16, 3, 4}, {20, 3, 8}, {32, 5, 8}, {48, 9, 4}} {
		im, err := png.Decode(bytes.NewReader(PNG(item.size, false, false)))
		if err != nil {
			t.Fatal(err)
		}
		groups, inside := 0, false
		for x := range item.size {
			white := color.NRGBAModel.Convert(im.At(x, item.row)).(color.NRGBA) == (color.NRGBA{R: 255, G: 255, B: 255, A: 255})
			if white && !inside {
				groups++
			}
			inside = white
		}
		if groups != item.groups {
			t.Fatalf("%dpx contact groups: got %d, want %d", item.size, groups, item.groups)
		}
	}
	for _, template := range []bool{false, true} {
		base, err := png.Decode(bytes.NewReader(PNG(20, false, template)))
		if err != nil {
			t.Fatal(err)
		}
		retina, err := png.Decode(bytes.NewReader(PNG(36, false, template)))
		if err != nil {
			t.Fatal(err)
		}
		for y := range 36 {
			for x := range 36 {
				want := color.NRGBAModel.Convert(base.At(x/2+1, y/2+1))
				if got := color.NRGBAModel.Convert(retina.At(x, y)); got != want {
					t.Fatalf("retina menu changed native cells at (%d,%d)", x, y)
				}
			}
		}
		for i := range 20 {
			for _, p := range [][2]int{{0, i}, {19, i}, {i, 0}, {i, 19}} {
				_, _, _, a := base.At(p[0], p[1]).RGBA()
				if a != 0 {
					t.Fatal("menu crop would discard artwork")
				}
			}
		}
	}
}

func TestExportedAssetsMatchCanonicalSource(t *testing.T) {
	files := map[string][]byte{"logo.svg": SVG(), "logo.png": PNG(1024, false, false), "app-icon.png": PNG(1024, true, false), "tray.png": PNG(36, false, true),
		"icon-16.png": PNG(16, false, false), "icon-20.png": PNG(20, false, false), "icon-32.png": PNG(32, false, false),
		"template-16.png": PNG(16, false, true), "template-20.png": PNG(20, false, true), "template-32.png": PNG(32, false, true)}
	for name, expected := range files {
		data, err := exported.ReadFile("assets/" + name)
		if err != nil || !bytes.Equal(data, expected) {
			t.Fatalf("regenerate %s after editing mark.svg: %v", name, err)
		}
	}
	data, err := exported.ReadFile("assets/app-icon.icns")
	if err != nil {
		t.Fatal(err)
	}
	if string(data[:4]) != "icns" || int(binary.BigEndian.Uint32(data[4:8])) != len(data) {
		t.Fatal("invalid ICNS container")
	}
	sizes := map[string]int{"icp4": 16, "icp5": 32, "icp6": 64, "ic07": 128, "ic08": 256, "ic09": 512, "ic10": 1024}
	for offset := 8; offset < len(data); {
		length := int(binary.BigEndian.Uint32(data[offset+4 : offset+8]))
		if length <= 8 || offset+length > len(data) {
			t.Fatal("invalid ICNS block")
		}
		size, ok := sizes[string(data[offset:offset+4])]
		if !ok || !bytes.Equal(data[offset+8:offset+length], PNG(size, true, false)) {
			t.Fatal("ICNS representation differs from native grid")
		}
		delete(sizes, string(data[offset:offset+4]))
		offset += length
	}
	if len(sizes) != 0 {
		t.Fatal("missing ICNS representation")
	}
	data, err = exported.ReadFile("assets/app-icon.ico")
	if err != nil {
		t.Fatal(err)
	}
	if binary.LittleEndian.Uint16(data[2:4]) != 1 {
		t.Fatal("invalid ICO container")
	}
	count := int(binary.LittleEndian.Uint16(data[4:6]))
	icoSizes := []int{16, 20, 32, 48, 64, 128, 256}
	if count != len(icoSizes) {
		t.Fatal("missing ICO representation")
	}
	for i, size := range icoSizes {
		entry := data[6+i*16 : 6+(i+1)*16]
		length, offset := int(binary.LittleEndian.Uint32(entry[8:12])), int(binary.LittleEndian.Uint32(entry[12:16]))
		if offset+length > len(data) {
			t.Fatal("invalid ICO offset")
		}
		if entry[0] != byte(size) || entry[1] != byte(size) || !bytes.Equal(data[offset:offset+length], PNG(size, true, false)) {
			t.Fatal("ICO representation differs from native grid")
		}
	}
}
