package transfer

import (
	"bytes"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"os"

	"golang.org/x/image/bmp"
	"golang.org/x/image/draw"
	"golang.org/x/image/webp"
)

// 缩略图规格:最长边 512px(JPEG q80 起步,含透明转 PNG),
// 128KB 只是防噪点图的保险上限,典型 25–45KB。
const (
	thumbMaxEdge  = 512
	thumbMaxBytes = 128 << 10
)

type thumbData struct {
	data []byte
	mime string
	w, h int
}

// makeThumbnail 为图片文件生成缩略图。解码/缩放/编码任一步失败都返回错误,
// 由上传管线选择忽略——缩略图绝不阻断上传。
func makeThumbnail(srcPath string) (thumbData, error) {
	var t thumbData
	f, err := os.Open(srcPath)
	if err != nil {
		return t, err
	}
	defer f.Close()

	// 先嗅探 512 字节确定类型,再 Seek 回头交给对应解码器
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return t, err
	}
	var img image.Image
	switch ct := http.DetectContentType(head); ct {
	case "image/jpeg":
		img, err = jpeg.Decode(f)
	case "image/png":
		img, err = png.Decode(f)
	case "image/gif":
		img, err = gif.Decode(f) // 只取首帧
	case "image/bmp":
		img, err = bmp.Decode(f)
	case "image/webp":
		img, err = webp.Decode(f)
	default:
		return t, fmt.Errorf("thumb: 非受支持的图片类型 %q", ct)
	}
	if err != nil {
		return t, err
	}

	img = fitEdge(img, thumbMaxEdge)
	return encodeThumb(img, thumbMaxBytes)
}

// fitEdge 等比缩放使最长边不超过 maxEdge;已达标则原样返回。
func fitEdge(img image.Image, maxEdge int) image.Image {
	b := img.Bounds()
	longest := max(b.Dx(), b.Dy())
	if longest <= maxEdge {
		return img
	}
	return scaleBy(img, float64(maxEdge)/float64(longest))
}

func scaleBy(img image.Image, s float64) image.Image {
	b := img.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, max(1, int(float64(b.Dx())*s)), max(1, int(float64(b.Dy())*s))))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Over, nil)
	return dst
}

// encodeThumb 编码并控制在预算内:含透明用 PNG(无质量旋钮,超限则再缩),
// 否则 JPEG 从 q80 逐级降到 q20,仍超限则缩 75% 重来。
func encodeThumb(img image.Image, maxBytes int) (thumbData, error) {
	if hasTransparency(img) {
		for range 8 { // 最多缩 8 轮,1x1 的 PNG 必然达标
			var buf bytes.Buffer
			if err := png.Encode(&buf, img); err != nil {
				return thumbData{}, err
			}
			if buf.Len() <= maxBytes {
				return thumbData{data: buf.Bytes(), mime: "image/png",
					w: img.Bounds().Dx(), h: img.Bounds().Dy()}, nil
			}
			img = scaleBy(img, 0.75)
		}
		return thumbData{}, fmt.Errorf("thumb: PNG 缩略图无法压入 %d 字节", maxBytes)
	}
	for round := 0; round < 8; round++ {
		for q := 80; q >= 20; q -= 15 {
			var buf bytes.Buffer
			if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
				return thumbData{}, err
			}
			if buf.Len() <= maxBytes {
				return thumbData{data: buf.Bytes(), mime: "image/jpeg",
					w: img.Bounds().Dx(), h: img.Bounds().Dy()}, nil
			}
		}
		img = scaleBy(img, 0.75) // 最低质量仍超限(极端噪点图):缩小再来
	}
	return thumbData{}, fmt.Errorf("thumb: JPEG 缩略图无法压入 %d 字节", maxBytes)
}

func hasTransparency(img image.Image) bool {
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a != 0xFFFF {
				return true
			}
		}
	}
	return false
}
