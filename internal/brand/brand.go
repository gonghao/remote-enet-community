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
)

const Accent = "#2356d8"

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
	case size == 18 || size == 36:
		// AppKit draws status images at 18 points. Remove only the 20-grid's
		// transparent margin so its 2x bitmap stays aligned to physical pixels.
		return artwork{grid: 18, start: 1, boxes: compact20.boxes}
	case size == 20:
		return compact20
	case size%32 == 0:
		return master
	default:
		return compact16
	}
}

// SVG returns a detached copy of the 32-unit master; compact grids are separate assets.
func SVG() []byte { return []byte(markSVG) }

// PNG accepts multiples of 16 up to 1024, plus 20 and AppKit's 18/36 menu sizes.
// Template uses black alpha with transparent cutouts and ignores plate.
func PNG(size int, plate, template bool) []byte {
	if size < 16 || size > 1024 || size%16 != 0 && size != 18 && size != 20 && size != 36 {
		panic("brand: invalid pixel icon size")
	}
	art := artworkFor(size)
	im := image.NewNRGBA(image.Rect(0, 0, size, size))
	if plate && !template {
		draw.Draw(im, im.Bounds(), image.NewUniform(color.NRGBA{R: 243, G: 245, B: 248, A: 255}), image.Point{}, draw.Src)
	}
	unit := size / art.grid
	for _, r := range art.boxes {
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
		box := image.Rect((r.X-art.start)*unit, (r.Y-art.start)*unit, (r.X+r.Width-art.start)*unit, (r.Y+r.Height-art.start)*unit)
		draw.Draw(im, box, image.NewUniform(fill), image.Point{}, draw.Src)
	}
	var data bytes.Buffer
	if err := png.Encode(&data, im); err != nil {
		panic(err)
	}
	return data.Bytes()
}
