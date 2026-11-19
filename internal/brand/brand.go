// Package brand renders the RJ45 Car identity with size-specific pixel grids.
package brand

import (
	"bytes"
	_ "embed"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
)

const Accent = "#2356d8"

var plateColor = color.NRGBA{R: 243, G: 245, B: 248, A: 255}

var (
	accentTop    = color.NRGBA{R: 0x42, G: 0x96, B: 0xf6, A: 255}
	accentBottom = color.NRGBA{R: 0x30, G: 0x34, B: 0xe8, A: 255}
)

// macPlateBottom is the accent mixed 25% into white.
var macPlateBottom = color.NRGBA{R: 200, G: 213, B: 245, A: 255}

//go:embed assets/mark.svg
var markSVG string

//go:embed assets/mark-16.svg
var mark16SVG string

//go:embed assets/mark-20.svg
var mark20SVG string

type rectangle struct {
	X      int    `xml:"x,attr"`
	Y      int    `xml:"y,attr"`
	Width  int    `xml:"width,attr"`
	Height int    `xml:"height,attr"`
	Fill   string `xml:"fill,attr"`
}

type artwork struct {
	grid  int
	start int
	boxes []rectangle
}

func parseSVG(data string) artwork {
	var source struct {
		ViewBox    string      `xml:"viewBox,attr"`
		Rectangles []rectangle `xml:"rect"`
	}
	if err := xml.Unmarshal([]byte(data), &source); err != nil {
		panic(err)
	}
	var x, y, width, height int
	if n, err := fmt.Sscanf(source.ViewBox, "%d %d %d %d", &x, &y, &width, &height); err != nil || n != 4 || x != 0 || y != 0 || width != height || width <= 0 {
		panic("brand: invalid SVG grid")
	}
	for _, r := range source.Rectangles {
		if r.X < 0 || r.Y < 0 || r.Width <= 0 || r.Height <= 0 || r.X+r.Width > width || r.Y+r.Height > height {
			panic("brand: source rectangle exceeds SVG grid")
		}
	}
	return artwork{grid: width, boxes: source.Rectangles}
}

var master = parseSVG(markSVG)
var compact16 = parseSVG(mark16SVG)
var compact20 = parseSVG(mark20SVG)

func artworkFor(size int) artwork {
	switch {
	case size == 20:
		return compact20
	case size%32 == 0:
		return master
	default:
		return compact16
	}
}

// SVG exports the color mark on the 32-unit grid.
func SVG() []byte {
	var data bytes.Buffer
	data.WriteString("<svg xmlns=\"http://www.w3.org/2000/svg\" width=\"512\" height=\"512\" viewBox=\"0 0 32 32\" shape-rendering=\"crispEdges\">\n")
	data.WriteString("  <title>Remote ENET — RJ45 Car, 32-unit optical master</title>\n")
	master.renderRects(false, func(r rectangle, fill color.NRGBA) {
		fmt.Fprintf(&data, "  <rect x=\"%d\" y=\"%d\" width=\"%d\" height=\"%d\" fill=\"#%02x%02x%02x\"/>\n",
			r.X, r.Y, r.Width, r.Height, fill.R, fill.G, fill.B)
	})
	data.WriteString("</svg>\n")
	return data.Bytes()
}

// PNG accepts multiples of 16 up to 1024, plus 20.
// Template uses black alpha with transparent cutouts and ignores plate.
func PNG(size int, plate, template bool) []byte {
	if size < 16 || size > 1024 || size%16 != 0 && size != 20 {
		panic("brand: invalid pixel icon size")
	}
	im := image.NewNRGBA(image.Rect(0, 0, size, size))
	art := artworkFor(size)
	paint(im, image.Point{}, art, size/art.grid, plate, template)
	return encode(im)
}

// MacPNG is the macOS app icon: Apple's 824 body (inset 100 on a 1024 canvas)
// as a light gradient plate, with the mark at 5/8 of the canvas. The corners
// are superellipse arcs (extent 270, exponent 3.5) approximating Apple's
// continuous 185 corner; a circular 185 arc reads visibly rounder in the Dock.
// A bundled .icns gets no system mask, so the rounded body and its margin must
// be drawn here. The body edge is anti-aliased; the mark stays on whole pixels.
// Size must be a multiple of 64 up to 1024.
func MacPNG(size int) []byte {
	if size < 64 || size > 1024 || size%64 != 0 {
		panic("brand: invalid macOS icon size")
	}
	im := image.NewNRGBA(image.Rect(0, 0, size, size))
	scale := float64(size) / 1024
	half, inset, corner := float64(size)/2, 100*scale, 270*scale
	for y := range size {
		for x := range size {
			qx := math.Abs(float64(x)+0.5-half) - (half - inset - corner)
			qy := math.Abs(float64(y)+0.5-half) - (half - inset - corner)
			d := math.Max(qx, qy) - corner
			if qx > 0 && qy > 0 {
				d = math.Pow(math.Pow(qx, 3.5)+math.Pow(qy, 3.5), 1/3.5) - corner
			}
			if alpha := math.Min(math.Max(0.5-d, 0), 1); alpha > 0 {
				c := macPlate(size, y)
				c.A = uint8(math.Round(alpha * 255))
				im.SetNRGBA(x, y, c)
			}
		}
	}
	mark, art := size*5/8, master
	if mark%master.grid != 0 {
		art = compact20 // 64 and 128: 40 and 80 px are whole 20-grid multiples
	}
	origin, unit := image.Pt((size-mark)/2, (size-mark)/2), mark/art.grid
	paint(im, origin, art, unit, false, false)
	return encode(im)
}

// accentForRow shades one grid row of the mark from azure at the top to indigo at
// the bottom. Whole rows keep each cell flat.
func accentForRow(row, top, bottom int) color.NRGBA {
	t := float64(row-top) / float64(max(1, bottom-1-top))
	mix := func(a, b uint8) uint8 { return uint8(math.Round(float64(a) + t*(float64(b)-float64(a)))) }
	return color.NRGBA{R: mix(accentTop.R, accentBottom.R), G: mix(accentTop.G, accentBottom.G), B: mix(accentTop.B, accentBottom.B), A: 255}
}

// accentRows returns the half-open grid row range covered by accent cells.
func (a artwork) accentRows() (top, bottom int) {
	top = a.grid
	for _, r := range a.boxes {
		if r.Fill == Accent {
			top, bottom = min(top, r.Y-a.start), max(bottom, r.Y+r.Height-a.start)
		}
	}
	return top, bottom
}

// macPlate fades the macOS body from white to a pale accent tint, top to
// bottom, matching the light gradients of neighbouring Dock icons.
func macPlate(size, y int) color.NRGBA {
	inset := float64(size) * 100 / 1024
	t := math.Min(math.Max((float64(y)+0.5-inset)/(float64(size)-2*inset), 0), 1)
	mix := func(to uint8) uint8 { return uint8(math.Round(255 - t*(255-float64(to)))) }
	return color.NRGBA{R: mix(macPlateBottom.R), G: mix(macPlateBottom.G), B: mix(macPlateBottom.B), A: 255}
}

func paint(im *image.NRGBA, origin image.Point, art artwork, unit int, plate, template bool) {
	if plate && !template {
		body := image.Rect(0, 0, art.grid*unit, art.grid*unit).Add(origin)
		draw.Draw(im, body, image.NewUniform(plateColor), image.Point{}, draw.Src)
	}
	art.renderRects(template, func(r rectangle, fill color.NRGBA) {
		box := image.Rect((r.X-art.start)*unit, (r.Y-art.start)*unit, (r.X+r.Width-art.start)*unit, (r.Y+r.Height-art.start)*unit).Add(origin)
		draw.Draw(im, box, image.NewUniform(fill), image.Point{}, draw.Src)
	})
}

func (a artwork) renderRects(template bool, emit func(rectangle, color.NRGBA)) {
	top, bottom := a.accentRows()
	for _, r := range a.boxes {
		if !template && r.Fill == Accent {
			band := r
			band.Height = 1
			for y := r.Y; y < r.Y+r.Height; y++ {
				band.Y = y
				emit(band, accentForRow(y-a.start, top, bottom))
			}
			continue
		}
		if len(r.Fill) != 7 || r.Fill[0] != '#' {
			panic("brand: invalid source color")
		}
		value, err := hex.DecodeString(r.Fill[1:])
		if err != nil || len(value) != 3 {
			panic("brand: invalid source color")
		}
		fill := color.NRGBA{R: value[0], G: value[1], B: value[2], A: 255}
		if template {
			if fill == (color.NRGBA{R: 255, G: 255, B: 255, A: 255}) {
				fill = color.NRGBA{}
			} else {
				fill = color.NRGBA{A: 255}
			}
		}
		emit(r, fill)
	}
}

func encode(im image.Image) []byte {
	var data bytes.Buffer
	if err := png.Encode(&data, im); err != nil {
		panic(err)
	}
	return data.Bytes()
}
