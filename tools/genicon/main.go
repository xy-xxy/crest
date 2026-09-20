// genicon - 生成 crest.ico
//
// 设计：取项目名 crest（浪峰 / 堰顶）之意 —— 深蓝到青的渐变圆角方底，
// 白色的两道水线：上面一道隆起成峰（crest 本义），下面一道趋于平缓，
// 一起读作"涌起的水"。峰形用高斯隆起 y = base - amp*exp(-((x-xc)/w)^2)，
// 两端自然收平，比正弦更像真实的单个浪头。
// 笔画由折线描边生成（到折线距离 <= 半宽即着色），端点天然圆头；
// 缩到 16px 仍是"两道弧线"，可辨识。
// 输出多分辨率(16/32/48/64/128/256)的 PNG 打包成 ICO(Vista+ 支持 PNG 嵌入)。
//
// 用法:
//   go run ./tools/genicon -o cmd/crest/crest.ico -syso cmd/crest/rsrc_windows.syso \
//       -png crest.png -png-size 200

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
	out := flag.String("o", "crest.ico", "输出 .ico 路径")
	pngOut := flag.String("png", "", "额外输出一个 PNG（用于 README，如 crest.png）")
	pngSize := flag.Int("png-size", 256, "PNG 边长")
	sysoOut := flag.String("syso", "", "输出 Windows 资源对象（如 cmd/crest/rsrc_windows.syso）")
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

	if *sysoOut != "" {
		if err := os.WriteFile(*sysoOut, buildSYSO(sizes, pngs), 0644); err != nil {
			panic(err)
		}
	}

	if *pngOut != "" {
		f, err := os.Create(*pngOut)
		if err != nil {
			panic(err)
		}
		if err := png.Encode(f, renderLogo(*pngSize)); err != nil {
			panic(err)
		}
		f.Close()
	}
}

// renderLogo 以 4x 超采样绘制后缩小，获得平滑边缘。
func renderLogo(size int) *image.NRGBA {
	const ss = 4
	big := size * ss
	src := image.NewNRGBA(image.Rect(0, 0, big, big))

	// 水的配色：自上而下由深蓝渐变到青
	top := color.NRGBA{0x0b, 0x4f, 0x7a, 0xff}
	bot := color.NRGBA{0x17, 0xa2, 0xc6, 0xff}
	fg := color.NRGBA{0xff, 0xff, 0xff, 0xff}

	f := float64(big)
	corner := f * 0.18

	// 两道水线：上为浪峰，下为余波
	waves := []wave{
		{f * 0.470, f * 0.175, f * 0.480, f * 0.200, f * 0.125, f * 0.875, f * 0.059},
		{f * 0.710, f * 0.072, f * 0.520, f * 0.230, f * 0.205, f * 0.795, f * 0.048},
	}
	strokes := make([][]point, 0, len(waves))
	const steps = 64
	for _, wv := range waves {
		pts := make([]point, 0, steps+1)
		for i := 0; i <= steps; i++ {
			x := wv.x0 + (wv.x1-wv.x0)*float64(i)/steps
			pts = append(pts, point{x, wv.at(x)})
		}
		strokes = append(strokes, pts)
	}

	for y := 0; y < big; y++ {
		py := float64(y) + 0.5
		// 背景渐变按行计算一次
		t := py / f
		bgRow := color.NRGBA{
			uint8(float64(top.R) + (float64(bot.R)-float64(top.R))*t),
			uint8(float64(top.G) + (float64(bot.G)-float64(top.G))*t),
			uint8(float64(top.B) + (float64(bot.B)-float64(top.B))*t),
			0xff,
		}
		for x := 0; x < big; x++ {
			px := float64(x) + 0.5
			if !inRoundedRect(px, py, f, corner) {
				continue
			}
			c := bgRow
			for i, pts := range strokes {
				if distToPolyline(px, py, pts) <= waves[i].half {
					c = fg
					break
				}
			}
			src.SetNRGBA(x, y, c)
		}
	}
	return downscale(src, size, ss)
}

// wave 是一道高斯隆起的水线：y(x) = base - amp*exp(-((x-xc)/w)^2)。
type wave struct {
	base, amp, xc, w, x0, x1, half float64
}

func (wv wave) at(x float64) float64 {
	t := (x - wv.xc) / wv.w
	return wv.base - wv.amp*math.Exp(-t*t)
}

type point struct{ x, y float64 }

// distToPolyline 返回点到折线的最短距离；折线描边天然产生圆头端点与平滑转角。
func distToPolyline(px, py float64, pts []point) float64 {
	best := math.Inf(1)
	for i := 0; i+1 < len(pts); i++ {
		if d := distToSegment(px, py, pts[i], pts[i+1]); d < best {
			best = d
		}
	}
	return best
}

func distToSegment(px, py float64, a, b point) float64 {
	vx, vy := b.x-a.x, b.y-a.y
	wx, wy := px-a.x, py-a.y
	l2 := vx*vx + vy*vy
	if l2 == 0 {
		return math.Hypot(wx, wy)
	}
	t := (wx*vx + wy*vy) / l2
	if t < 0 {
		t = 0
	} else if t > 1 {
		t = 1
	}
	return math.Hypot(px-(a.x+t*vx), py-(a.y+t*vy))
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

// ---- Windows 资源对象（.syso）----------------------------------------
//
// Windows exe 的图标并不来自 .ico 文件，而来自链接进二进制的 COFF 资源对象
// rsrc_windows.syso。这里手写该对象，省去 rsrc / goversioninfo 这类外部工具
// ——与项目"无外部依赖"的原则一致，也让图标能随 genicon 一起重建。
//
// 结构：COFF 头 + 一个 .rsrc 节 + 重定位表 + 符号表。节内是标准的 PE 资源树：
//   根目录 → RT_ICON(3)   → 每个尺寸一项 → 语言项 → 数据项 → PNG
//          → RT_GROUP_ICON(14) → 一项      → 语言项 → 数据项 → GRPICONDIR
// 数据项里的 OffsetToData 是最终 PE 中的 RVA，由链接器按 ADDR32NB 重定位填上，
// 我们只写节内偏移作为加数。

const (
	rtIcon        = 3
	rtGroupIcon   = 14
	langNeutral   = 0x0409
	imgRelAmd64NB = 0x0003 // IMAGE_REL_AMD64_ADDR32NB
)

func buildSYSO(sizes []int, pngs [][]byte) []byte {
	n := len(sizes)

	// ---- 先排布节内偏移 ----
	// 目录区：根(16+2*8) + RT_ICON 目录(16+n*8) + RT_GROUP_ICON 目录(16+8)
	//         + 每个图标的名称目录(16+8) + 组图标的名称目录(16+8)
	off := 0
	rootOff := off
	off += 16 + 2*8
	iconDirOff := off
	off += 16 + n*8
	groupDirOff := off
	off += 16 + 8
	iconNameOff := make([]int, n)
	for i := 0; i < n; i++ {
		iconNameOff[i] = off
		off += 16 + 8
	}
	groupNameOff := off
	off += 16 + 8

	// 数据项区（每项 16 字节）
	dataEntryOff := make([]int, n+1) // 0..n-1 为图标，n 为组图标
	for i := 0; i <= n; i++ {
		dataEntryOff[i] = off
		off += 16
	}

	// 实际数据区，逐块 8 字节对齐
	align8 := func(v int) int { return (v + 7) &^ 7 }
	blobOff := make([]int, n+1)
	for i := 0; i < n; i++ {
		off = align8(off)
		blobOff[i] = off
		off += len(pngs[i])
	}
	group := buildGroupIcon(sizes, pngs)
	off = align8(off)
	blobOff[n] = off
	off += len(group)
	sectionSize := off

	// ---- 写节内容 ----
	sec := make([]byte, sectionSize)
	put16 := func(at int, v uint16) { binary.LittleEndian.PutUint16(sec[at:], v) }
	put32 := func(at int, v uint32) { binary.LittleEndian.PutUint32(sec[at:], v) }

	// 目录头：Characteristics/TimeDateStamp/Major/Minor 全 0，随后是命名项数、ID 项数
	writeDir := func(at, idCount int) { put16(at+14, uint16(idCount)) }
	// 目录项：ID + 偏移（最高位为 1 表示指向子目录）
	writeEntry := func(at int, id uint32, target int, isDir bool) {
		put32(at, id)
		v := uint32(target)
		if isDir {
			v |= 0x80000000
		}
		put32(at+4, v)
	}

	writeDir(rootOff, 2)
	writeEntry(rootOff+16, rtIcon, iconDirOff, true)
	writeEntry(rootOff+16+8, rtGroupIcon, groupDirOff, true)

	writeDir(iconDirOff, n)
	for i := 0; i < n; i++ {
		writeEntry(iconDirOff+16+i*8, uint32(i+1), iconNameOff[i], true)
	}
	writeDir(groupDirOff, 1)
	writeEntry(groupDirOff+16, 1, groupNameOff, true)

	for i := 0; i < n; i++ {
		writeDir(iconNameOff[i], 1)
		writeEntry(iconNameOff[i]+16, langNeutral, dataEntryOff[i], false)
	}
	writeDir(groupNameOff, 1)
	writeEntry(groupNameOff+16, langNeutral, dataEntryOff[n], false)

	// 数据项：OffsetToData（待重定位）、Size、CodePage、Reserved
	for i := 0; i < n; i++ {
		put32(dataEntryOff[i], uint32(blobOff[i]))
		put32(dataEntryOff[i]+4, uint32(len(pngs[i])))
		copy(sec[blobOff[i]:], pngs[i])
	}
	put32(dataEntryOff[n], uint32(blobOff[n]))
	put32(dataEntryOff[n]+4, uint32(len(group)))
	copy(sec[blobOff[n]:], group)

	// ---- 组装 COFF ----
	var buf bytes.Buffer
	w := func(v any) { binary.Write(&buf, binary.LittleEndian, v) }

	numReloc := n + 1
	const coffHdr, secHdr, relocSize, symSize = 20, 40, 10, 18
	secDataPtr := coffHdr + secHdr
	relocPtr := secDataPtr + sectionSize
	symPtr := relocPtr + numReloc*relocSize

	w(uint16(0x8664)) // Machine: AMD64（build.sh 只出 windows/amd64）
	w(uint16(1))      // NumberOfSections
	w(uint32(0))      // TimeDateStamp
	w(uint32(symPtr)) // PointerToSymbolTable
	w(uint32(1))      // NumberOfSymbols
	w(uint16(0))      // SizeOfOptionalHeader
	w(uint16(0))      // Characteristics

	buf.Write([]byte(".rsrc\x00\x00\x00"))
	w(uint32(0))           // VirtualSize
	w(uint32(0))           // VirtualAddress
	w(uint32(sectionSize)) // SizeOfRawData
	w(uint32(secDataPtr))  // PointerToRawData
	w(uint32(relocPtr))    // PointerToRelocations
	w(uint32(0))           // PointerToLinenumbers
	w(uint16(numReloc))    // NumberOfRelocations
	w(uint16(0))           // NumberOfLinenumbers
	w(uint32(0x40000040))  // INITIALIZED_DATA | MEM_READ

	buf.Write(sec)

	for i := 0; i <= n; i++ {
		w(uint32(dataEntryOff[i])) // 要修正的字段位置
		w(uint32(0))               // 符号表索引：.rsrc 节符号
		w(uint16(imgRelAmd64NB))
	}

	buf.Write([]byte(".rsrc\x00\x00\x00"))
	w(uint32(0)) // Value
	w(int16(1))  // SectionNumber
	w(uint16(0)) // Type
	w(uint8(3))  // StorageClass: STATIC
	w(uint8(0))  // NumberOfAuxSymbols
	w(uint32(4)) // 字符串表：仅长度字段

	return buf.Bytes()
}

// buildGroupIcon 生成 RT_GROUP_ICON 资源（GRPICONDIR）。它与 .ico 的目录几乎
// 相同，区别是每项末尾是 2 字节的资源 ID 而非 4 字节的文件偏移。
func buildGroupIcon(sizes []int, pngs [][]byte) []byte {
	var buf bytes.Buffer
	w := func(v any) { binary.Write(&buf, binary.LittleEndian, v) }
	w(uint16(0)) // Reserved
	w(uint16(1)) // Type: icon
	w(uint16(len(sizes)))
	for i, s := range sizes {
		dim := uint8(s)
		if s >= 256 {
			dim = 0
		}
		buf.WriteByte(dim) // Width
		buf.WriteByte(dim) // Height
		buf.WriteByte(0)   // ColorCount
		buf.WriteByte(0)   // Reserved
		w(uint16(1))       // Planes
		w(uint16(32))      // BitCount
		w(uint32(len(pngs[i])))
		w(uint16(i + 1)) // 资源 ID，对应 RT_ICON 下的条目
	}
	return buf.Bytes()
}
