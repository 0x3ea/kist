package transfer

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

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

// errNotImage 标记"嗅探后不是受支持的图片类型":非图片文件属预期而非故障,
// 上传侧据此静默跳过,只有"是图片但生成失败"才记 Warn 日志(TODO-07)。
var errNotImage = errors.New("thumb: 非受支持的图片类型")

// ThumbData 是缩略图的编码产物;导出供 GUI 封面导入复用(TODO-17)——
// 手动封面与上传缩略图同一规格,不另起一套。
type ThumbData struct {
	Data []byte
	Mime string
	W, H int
}

// MakeThumbnail 为图片文件生成缩略图(epub 例外:封面从包内抽取,见 epub.go)。
// 解码/缩放/编码任一步失败都返回错误:上传管线据此选择忽略(缩略图绝不阻断
// 上传);GUI 封面导入则是显式用户动作,错误应原样上抛而非静默——两种策略
// 都在调用方。
func MakeThumbnail(srcPath string) (ThumbData, error) {
	// epub 是 zip 容器,内容嗅探只会得到 application/zip:按扩展名分流
	if strings.EqualFold(filepath.Ext(srcPath), ".epub") {
		return epubThumbnail(srcPath)
	}

	var t ThumbData
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
	img, err := decodeImage(head, f)
	if err != nil {
		return t, err
	}
	return encodeThumb(fitEdge(img, thumbMaxEdge), thumbMaxBytes)
}

// decodeImage 嗅探并解码图片:head 是开头若干字节(≥512 最佳,供
// DetectContentType 判型),r 是完整字节流。不受支持的类型返回 errNotImage
func decodeImage(head []byte, r io.Reader) (image.Image, error) {
	switch ct := http.DetectContentType(head); ct {
	case "image/jpeg":
		return jpeg.Decode(r)
	case "image/png":
		return png.Decode(r)
	case "image/gif":
		return gif.Decode(r) // 只取首帧
	case "image/bmp":
		return bmp.Decode(r)
	case "image/webp":
		return webp.Decode(r)
	default:
		return nil, fmt.Errorf("%w %q", errNotImage, ct)
	}
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
func encodeThumb(img image.Image, maxBytes int) (ThumbData, error) {
	if hasTransparency(img) {
		for range 8 { // 最多缩 8 轮,1x1 的 PNG 必然达标
			var buf bytes.Buffer
			if err := png.Encode(&buf, img); err != nil {
				return ThumbData{}, err
			}
			if buf.Len() <= maxBytes {
				return ThumbData{Data: buf.Bytes(), Mime: "image/png",
					W: img.Bounds().Dx(), H: img.Bounds().Dy()}, nil
			}
			img = scaleBy(img, 0.75)
		}
		return ThumbData{}, fmt.Errorf("thumb: PNG 缩略图无法压入 %d 字节", maxBytes)
	}
	for round := 0; round < 8; round++ {
		for q := 80; q >= 20; q -= 15 {
			var buf bytes.Buffer
			if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
				return ThumbData{}, err
			}
			if buf.Len() <= maxBytes {
				return ThumbData{Data: buf.Bytes(), Mime: "image/jpeg",
					W: img.Bounds().Dx(), H: img.Bounds().Dy()}, nil
			}
		}
		img = scaleBy(img, 0.75) // 最低质量仍超限(极端噪点图):缩小再来
	}
	return ThumbData{}, fmt.Errorf("thumb: JPEG 缩略图无法压入 %d 字节", maxBytes)
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
