// 生成 icon.ico：米色表盘 + 蓝色圆环 + 10:10 经典指针。
// 运行方式：go run ./tools/genicon
// 产物：项目根目录下的 icon.ico。
package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
)

var (
	ringColor   = color.RGBA{48, 110, 200, 255}  // 蓝色圆环
	faceColor   = color.RGBA{246, 232, 200, 255} // 米色表盘
	handColor   = color.RGBA{28, 40, 72, 255}    // 深海军蓝指针
	tickColor   = color.RGBA{120, 95, 55, 255}   // 棕色刻度
	shadowColor = color.RGBA{0, 0, 0, 40}        // 表盘内圈轻微阴影
)

// drawIcon 画一个 size×size 的时钟图标，透明圆外。
func drawIcon(size int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, size, size))

	cx := float64(size-1) / 2
	cy := float64(size-1) / 2
	outerR := float64(size)/2 - 0.5
	ringThick := math.Max(1.5, float64(size)*0.08)
	faceR := outerR - ringThick - math.Max(0.5, float64(size)*0.01)

	// 表盘 + 圆环
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx := float64(x) - cx
			dy := float64(y) - cy
			d := math.Sqrt(dx*dx + dy*dy)
			if d > outerR+0.5 {
				continue // 圆外透明
			}
			if d > outerR-0.5 {
				// 外缘抗锯齿
				a := clamp01(outerR + 0.5 - d)
				setPx(img, x, y, fade(ringColor, a))
				continue
			}
			if d > faceR {
				// 圆环内部实色
				setPx(img, x, y, ringColor)
				continue
			}
			if d > faceR-0.5 {
				// 环-面边界抗锯齿：过渡 ring → face
				t := clamp01(faceR + 0.5 - d)
				setPx(img, x, y, lerp(ringColor, faceColor, t))
				continue
			}
			// 表盘米色
			setPx(img, x, y, faceColor)
		}
	}

	// 轻微的内阴影，让表盘有立体感（仅在 32+ 时画）
	if size >= 32 {
		for y := 0; y < size; y++ {
			for x := 0; x < size; x++ {
				dx := float64(x) - cx
				dy := float64(y) - cy
				d := math.Sqrt(dx*dx + dy*dy)
				if d > faceR-0.5 || d < faceR-math.Max(1, float64(size)*0.03) {
					continue
				}
				blend(img, x, y, shadowColor)
			}
		}
	}

	// 12 / 3 / 6 / 9 位置的刻度点（尺寸大时画）
	if size >= 24 {
		tickR := faceR * 0.82
		tickSize := math.Max(0.8, float64(size)*0.04)
		for i := 0; i < 12; i++ {
			// 角度：从 12 点开始顺时针，每 30 度一格
			ang := -math.Pi/2 + float64(i)*math.Pi/6
			tx := cx + tickR*math.Cos(ang)
			ty := cy + tickR*math.Sin(ang)
			radius := tickSize
			if i%3 == 0 {
				radius = tickSize * 1.4 // 主刻度更粗
			}
			fillDisk(img, tx, ty, radius, tickColor)
		}
	}

	// 指针：10:10 经典位置
	// 屏幕坐标 +y 向下；12 点方向 = (0, -1)
	// 顺时针 θ 度的方向：(sin θ, -cos θ)
	// 时针在 10 点：(10/12)*360 = 300° = 5π/3
	// 分针在 10 分：(10/60)*360 = 60° = π/3
	hourAng := 5 * math.Pi / 3
	minAng := math.Pi / 3
	hourLen := faceR * 0.50
	minLen := faceR * 0.72
	hourThick := math.Max(1.2, float64(size)*0.07)
	minThick := math.Max(1.0, float64(size)*0.055)

	drawHand(img, cx, cy,
		cx+hourLen*math.Sin(hourAng), cy-hourLen*math.Cos(hourAng),
		hourThick, handColor)
	drawHand(img, cx, cy,
		cx+minLen*math.Sin(minAng), cy-minLen*math.Cos(minAng),
		minThick, handColor)

	// 中心钉
	centerR := math.Max(1.0, float64(size)*0.05)
	fillDisk(img, cx, cy, centerR, handColor)

	return img
}

func drawHand(img *image.RGBA, x1, y1, x2, y2, thick float64, c color.RGBA) {
	bounds := img.Bounds()
	half := thick / 2
	minX := int(math.Floor(math.Min(x1, x2) - thick))
	maxX := int(math.Ceil(math.Max(x1, x2) + thick))
	minY := int(math.Floor(math.Min(y1, y2) - thick))
	maxY := int(math.Ceil(math.Max(y1, y2) + thick))
	if minX < bounds.Min.X {
		minX = bounds.Min.X
	}
	if maxX > bounds.Max.X-1 {
		maxX = bounds.Max.X - 1
	}
	if minY < bounds.Min.Y {
		minY = bounds.Min.Y
	}
	if maxY > bounds.Max.Y-1 {
		maxY = bounds.Max.Y - 1
	}
	dx := x2 - x1
	dy := y2 - y1
	len2 := dx*dx + dy*dy
	if len2 == 0 {
		return
	}
	for y := minY; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			px := float64(x) - x1
			py := float64(y) - y1
			t := (px*dx + py*dy) / len2
			if t < 0 {
				t = 0
			}
			if t > 1 {
				t = 1
			}
			nx := x1 + t*dx
			ny := y1 + t*dy
			d := math.Sqrt((float64(x)-nx)*(float64(x)-nx) + (float64(y)-ny)*(float64(y)-ny))
			if d > half+0.5 {
				continue
			}
			a := clamp01(half + 0.5 - d)
			blend(img, x, y, color.RGBA{c.R, c.G, c.B, uint8(float64(c.A) * a)})
		}
	}
}

func fillDisk(img *image.RGBA, cx, cy, r float64, c color.RGBA) {
	bounds := img.Bounds()
	minX := int(math.Floor(cx - r - 1))
	maxX := int(math.Ceil(cx + r + 1))
	minY := int(math.Floor(cy - r - 1))
	maxY := int(math.Ceil(cy + r + 1))
	if minX < bounds.Min.X {
		minX = bounds.Min.X
	}
	if maxX > bounds.Max.X-1 {
		maxX = bounds.Max.X - 1
	}
	if minY < bounds.Min.Y {
		minY = bounds.Min.Y
	}
	if maxY > bounds.Max.Y-1 {
		maxY = bounds.Max.Y - 1
	}
	for y := minY; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			d := math.Sqrt((float64(x)-cx)*(float64(x)-cx) + (float64(y)-cy)*(float64(y)-cy))
			if d > r+0.5 {
				continue
			}
			a := clamp01(r + 0.5 - d)
			blend(img, x, y, color.RGBA{c.R, c.G, c.B, uint8(float64(c.A) * a)})
		}
	}
}

func setPx(img *image.RGBA, x, y int, c color.RGBA) {
	img.SetRGBA(x, y, c)
}

func blend(img *image.RGBA, x, y int, over color.RGBA) {
	if over.A == 0 {
		return
	}
	base := img.RGBAAt(x, y)
	ao := float64(over.A) / 255
	ab := float64(base.A) / 255
	a := ao + ab*(1-ao)
	if a == 0 {
		img.SetRGBA(x, y, color.RGBA{})
		return
	}
	r := (float64(over.R)*ao + float64(base.R)*ab*(1-ao)) / a
	g := (float64(over.G)*ao + float64(base.G)*ab*(1-ao)) / a
	b := (float64(over.B)*ao + float64(base.B)*ab*(1-ao)) / a
	img.SetRGBA(x, y, color.RGBA{R: uint8(r), G: uint8(g), B: uint8(b), A: uint8(a * 255)})
}

func fade(c color.RGBA, a float64) color.RGBA {
	return color.RGBA{c.R, c.G, c.B, uint8(float64(c.A) * a)}
}

func lerp(a, b color.RGBA, t float64) color.RGBA {
	return color.RGBA{
		R: uint8(float64(a.R)*(1-t) + float64(b.R)*t),
		G: uint8(float64(a.G)*(1-t) + float64(b.G)*t),
		B: uint8(float64(a.B)*(1-t) + float64(b.B)*t),
		A: uint8(float64(a.A)*(1-t) + float64(b.A)*t),
	}
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// 打包成 .ico 文件：每个尺寸作为一个 PNG 条目。
// Windows 7+ 均支持 PNG-in-ICO（包括小尺寸）。
func writeICO(path string, sizes []int) error {
	type entry struct {
		w, h int
		png  []byte
	}
	entries := make([]entry, len(sizes))
	for i, s := range sizes {
		img := drawIcon(s)
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			return err
		}
		entries[i] = entry{s, s, buf.Bytes()}
	}

	var out bytes.Buffer
	binary.Write(&out, binary.LittleEndian, uint16(0))            // reserved
	binary.Write(&out, binary.LittleEndian, uint16(1))            // type=1 (ICO)
	binary.Write(&out, binary.LittleEndian, uint16(len(entries))) // count

	offset := 6 + 16*len(entries)
	for _, e := range entries {
		w := byte(e.w)
		h := byte(e.h)
		if e.w >= 256 {
			w = 0
		}
		if e.h >= 256 {
			h = 0
		}
		binary.Write(&out, binary.LittleEndian, w)
		binary.Write(&out, binary.LittleEndian, h)
		binary.Write(&out, binary.LittleEndian, uint8(0))            // color count
		binary.Write(&out, binary.LittleEndian, uint8(0))            // reserved
		binary.Write(&out, binary.LittleEndian, uint16(1))           // planes
		binary.Write(&out, binary.LittleEndian, uint16(32))          // bpp
		binary.Write(&out, binary.LittleEndian, uint32(len(e.png)))  // size
		binary.Write(&out, binary.LittleEndian, uint32(offset))      // offset
		offset += len(e.png)
	}
	for _, e := range entries {
		out.Write(e.png)
	}
	return os.WriteFile(path, out.Bytes(), 0644)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "preview" {
		for _, s := range []int{64, 128, 256} {
			img := drawIcon(s)
			f, err := os.Create("preview_" + itoa(s) + ".png")
			if err != nil {
				panic(err)
			}
			png.Encode(f, img)
			f.Close()
		}
		return
	}
	sizes := []int{16, 24, 32, 48, 64, 128, 256}
	if err := writeICO("icon.ico", sizes); err != nil {
		panic(err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}
