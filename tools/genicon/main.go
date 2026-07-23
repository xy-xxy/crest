// genicon - 生成 gonx.ico
//
// 设计：nginx 绿(#009639)圆角方底 + 白色几何 "G"（带缺口的圆环 + 水平横杆）。
// 输出多分辨率(16/32/48/64/128/256)的 PNG 打包成 ICO(Vista+ 支持 PNG 嵌入)。
//
// 用法:
//   go run ./tools/genicon -o cmd/gonx/gonx.ico

package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
)

func main() {
	out := flag.String("o", "gonx.ico", "输出 .ico 路径")
	flag.Parse()

	sizes := []int{16, 32, 48, 64, 128, 256}
	pngs := make([][]byte, 0, len(sizes))
	for _, s := range sizes {
		img := renderLogo(s)
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			panic(err)
		}
		pngs = append(pngs, buf.Bytes())
	}
	ico := buildICO(sizes, pngs)
	if err := os.WriteFile(*out, ico, 0644); err != nil {
		panic(err)
	}
}

// renderLogo 以 4x 超采样绘制后缩小，获得平滑边缘。
func renderLogo(size int) *image.NRGBA {
	const ss = 4
	big := size * ss
	src := image.NewNRGBA(image.Rect(0, 0, big, big))

	bg := color.NRGBA{0x00, 0x96, 0x39, 0xff} // nginx green
	fg := color.NRGBA{0xff, 0xff, 0xff, 0xff}

	f := float64(big)
	cx, cy := f/2, f/2
	corner := f * 0.18
	outerR := f * 0.335
	innerR := f * 0.185
	barHalf := f * 0.075
	// "G" 缺口：右侧 ±40°
	gapHalf := 40 * math.Pi / 180

	for y := 0; y < big; y++ {
		for x := 0; x < big; x++ {
			px, py := float64(x)+0.5, float64(y)+0.5
			if !inRoundedRect(px, py, f, corner) {
				continue
			}
			c := bg
			dx, dy := px-cx, py-cy
			r := math.Hypot(dx, dy)
			ang := math.Atan2(dy, dx) // 0 = 正右方
			inGap := ang > -gapHalf && ang < gapHalf
			if r >= innerR && r <= outerR && !inGap {
				c = fg
			}
			// 横杆：从圆心向右伸入缺口，构成 G 的钩
			if dy >= -barHalf && dy <= barHalf && dx >= 0 && r <= outerR {
				c = fg
			}
			src.SetNRGBA(x, y, c)
		}
	}
	return downscale(src, size, ss)
}

func inRoundedRect(px, py, size, radius float64) bool {
	if px < 0 || py < 0 || px > size || py > size {
		return false
	}
	x := math.Max(math.Max(radius-px, px-(size-radius)), 0)
	y := math.Max(math.Max(radius-py, py-(size-radius)), 0)
	return math.Hypot(x, y) <= radius
}

func downscale(src *image.NRGBA, size, ss int) *image.NRGBA {
	dst := image.NewNRGBA(image.Rect(0, 0, size, size))
	n := ss * ss
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var r, g, b, a int
			for dy := 0; dy < ss; dy++ {
				for dx := 0; dx < ss; dx++ {
					c := src.NRGBAAt(x*ss+dx, y*ss+dy)
					r += int(c.R)
					g += int(c.G)
					b += int(c.B)
					a += int(c.A)
				}
			}
			dst.SetNRGBA(x, y, color.NRGBA{
				uint8(r / n), uint8(g / n), uint8(b / n), uint8(a / n),
			})
		}
	}
	return dst
}

// buildICO 将 PNG 图像打包为 ICO 容器。
func buildICO(sizes []int, pngs [][]byte) []byte {
	var buf bytes.Buffer
	w := func(v any) { binary.Write(&buf, binary.LittleEndian, v) }

	w(uint16(0)) // reserved
	w(uint16(1)) // type: icon
	w(uint16(len(sizes)))

	offset := 6 + 16*len(sizes)
	for i, s := range sizes {
		dim := uint8(s)
		if s >= 256 {
			dim = 0 // 0 表示 256
		}
		buf.WriteByte(dim)      // width
		buf.WriteByte(dim)      // height
		buf.WriteByte(0)        // palette
		buf.WriteByte(0)        // reserved
		w(uint16(1))            // color planes
		w(uint16(32))           // bpp
		w(uint32(len(pngs[i]))) // data size
		w(uint32(offset))       // data offset
		offset += len(pngs[i])
	}
	for _, p := range pngs {
		buf.Write(p)
	}
	return buf.Bytes()
}
