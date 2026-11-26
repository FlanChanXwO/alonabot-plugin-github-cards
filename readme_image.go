package plugin

import (
	"bytes"
	_ "embed"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"strings"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

//go:embed fonts/NotoSansCJKsc-Regular.otf
var noto []byte

var readmeFont = sync.OnceValues(func() (*opentype.Font, error) { return opentype.Parse(noto) })

func renderReadme(text string) ([][]byte, error) {
	parsed, err := readmeFont()
	if err != nil {
		return nil, err
	}
	face, err := opentype.NewFace(parsed, &opentype.FaceOptions{Size: 19, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return nil, err
	}
	defer face.Close()
	const width, pad, lineHeight, pageLines = 940, 32, 29, 48
	var lines []string
	for _, line := range strings.Split(strings.ReplaceAll(text, "\t", "    "), "\n") {
		var part strings.Builder
		var used fixed.Int26_6
		var previous rune
		for _, r := range line {
			advance, _ := face.GlyphAdvance(r)
			kern := face.Kern(previous, r)
			if used+advance+kern > fixed.I(width-2*pad) && part.Len() > 0 {
				lines = append(lines, part.String())
				part.Reset()
				used, kern = 0, 0
			}
			part.WriteRune(r)
			used += advance + kern
			previous = r
		}
		lines = append(lines, part.String())
	}
	var out [][]byte
	for len(lines) > 0 {
		n := min(len(lines), pageLines)
		img := image.NewRGBA(image.Rect(0, 0, width, 2*pad+n*lineHeight))
		draw.Draw(img, img.Bounds(), image.NewUniform(color.RGBA{246, 248, 250, 255}), image.Point{}, draw.Src)
		d := font.Drawer{Dst: img, Src: image.NewUniform(color.RGBA{31, 35, 40, 255}), Face: face}
		for i, line := range lines[:n] {
			d.Dot = fixed.P(pad, pad+(i+1)*lineHeight-face.Metrics().Descent.Ceil())
			d.DrawString(line)
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			return nil, err
		}
		out = append(out, buf.Bytes())
		lines = lines[n:]
	}
	return out, nil
}
