package brand

import (
	"bytes"
	"embed"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"testing"
)

//go:embed assets/*
var exported embed.FS

func TestPixelGridAndTemplateCutouts(t *testing.T) {
	for _, size := range []int{16, 20, 32, 48, 128, 256} {
		plain, err := png.Decode(bytes.NewReader(PNG(size, false, false)))
		if err != nil {
			t.Fatal(err)
		}
		mono, err := png.Decode(bytes.NewReader(PNG(size, false, true)))
		if err != nil {
			t.Fatal(err)
		}
		plated, err := png.Decode(bytes.NewReader(PNG(size, true, false)))
		if err != nil {
			t.Fatal(err)
		}
		if plain.Bounds().Dx() != size || plain.Bounds().Dy() != size {
			t.Fatal("invalid dimensions")
		}
		art := artworkFor(size)
		unit := size / art.grid
		top, bottom := art.accentRows()
		var firstAccent, lastAccent color.NRGBA
		for y := range size {
			row := y / unit
			for x := range size {
				pixel := color.NRGBAModel.Convert(plain.At(x, y)).(color.NRGBA)
				var expectedTemplate color.NRGBA
				switch pixel {
				case color.NRGBA{R: 255, G: 255, B: 255, A: 255}, color.NRGBA{}:
				default:
					if row < top || row >= bottom || pixel != accentForRow(row, top, bottom) {
						t.Fatalf("unexpected accent at %dpx (%d,%d): %v", size, x, y, pixel)
					}
					expectedTemplate.A = 255
					if row == top {
						firstAccent = pixel
					}
					if row == bottom-1 {
						lastAccent = pixel
					}
				}
				if got := color.NRGBAModel.Convert(mono.At(x, y)).(color.NRGBA); got != expectedTemplate {
					t.Fatalf("template lost a cutout or has color at %dpx (%d,%d): %v", size, x, y, got)
				}
				if pixel == (color.NRGBA{}) {
					pixel = plateColor
				}
				if got := color.NRGBAModel.Convert(plated.At(x, y)).(color.NRGBA); got != pixel {
					t.Fatalf("incorrect plate or foreground at %dpx (%d,%d): %v", size, x, y, got)
				}
			}
		}
		if firstAccent != (color.NRGBA{R: 0x42, G: 0x96, B: 0xf6, A: 255}) || lastAccent != (color.NRGBA{R: 0x30, G: 0x34, B: 0xe8, A: 255}) {
			t.Fatalf("%dpx: accent endpoints are %v and %v", size, firstAccent, lastAccent)
		}
		if !bytes.Equal(PNG(size, true, true), PNG(size, false, true)) {
			t.Fatal("template must not gain an opaque plate")
		}
	}
}

func TestSVGMatchesColorPNG(t *testing.T) {
	art := parseSVG(string(SVG()))
	for _, rect := range art.boxes {
		if rect.Fill == Accent {
			t.Fatal("exported SVG still uses the geometry source accent")
		}
	}
	raster := image.NewNRGBA(image.Rect(0, 0, master.grid, master.grid))
	paint(raster, image.Point{}, art, 1, false, false)
	if !bytes.Equal(encode(raster), PNG(master.grid, false, false)) {
		t.Fatal("SVG rectangles differ from the color PNG grid")
	}
}

func TestCompactGrids(t *testing.T) {
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
}

func TestExportedAssetsMatchCanonicalSource(t *testing.T) {
	files := map[string][]byte{"logo.svg": SVG(), "logo.png": PNG(1024, false, false), "app-icon.png": PNG(1024, true, false), "app-icon-macos.png": MacPNG(1024),
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
	sizes := map[string]int{"icp6": 64, "ic07": 128, "ic08": 256, "ic09": 512, "ic10": 1024}
	for offset := 8; offset < len(data); {
		length := int(binary.BigEndian.Uint32(data[offset+4 : offset+8]))
		if length <= 8 || offset+length > len(data) {
			t.Fatal("invalid ICNS block")
		}
		size, ok := sizes[string(data[offset:offset+4])]
		if !ok || !bytes.Equal(data[offset+8:offset+length], MacPNG(size)) {
			t.Fatal("ICNS representation differs from the macOS icon")
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

func TestMacRoundedBody(t *testing.T) {
	for _, size := range []int{64, 128, 256, 512, 1024} {
		mac, err := png.Decode(bytes.NewReader(MacPNG(size)))
		if err != nil {
			t.Fatal(err)
		}
		at := func(x, y int) color.NRGBA { return color.NRGBAModel.Convert(mac.At(x, y)).(color.NRGBA) }
		inset := size * 100 / 1024
		for _, p := range [][2]int{{0, 0}, {inset - 1, size / 2}, {size / 2, size - inset}, {inset + 2, inset + 2}, {size - inset - 3, size - inset - 3}} {
			if at(p[0], p[1]).A != 0 {
				t.Fatalf("%dpx: margin or rounded corner is opaque at %v", size, p)
			}
		}
		if at(inset+1, size/2) != macPlate(size, size/2) || at(size/2, inset+1) != macPlate(size, inset+1) {
			t.Fatalf("%dpx: rounded body lost its plate", size)
		}
		top, bottom := at(size/2, inset+1), at(size/2, size-inset-2)
		if top.B != 255 || !(bottom.R < top.R && bottom.G < top.G && bottom.B < top.B) {
			t.Fatalf("%dpx: plate must fade from white at the top to a tint at the bottom: %v -> %v", size, top, bottom)
		}
		side, base := size*5/8, PNG(20, false, false)
		if side%32 == 0 {
			base = PNG(side, false, false)
		}
		mark, err := png.Decode(bytes.NewReader(base))
		if err != nil {
			t.Fatal(err)
		}
		unit, offset := side/mark.Bounds().Dx(), (size-side)/2
		for y := range side {
			for x := range side {
				want := color.NRGBAModel.Convert(mark.At(x/unit, y/unit)).(color.NRGBA)
				if want == (color.NRGBA{}) {
					want = macPlate(size, y+offset)
				}
				if got := at(x+offset, y+offset); got != want {
					t.Fatalf("%dpx: centred mark differs at (%d,%d)", size, x, y)
				}
			}
		}
	}
}
