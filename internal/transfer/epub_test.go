package transfer

// epub_test.go — epub 封面抽取的边界用例:三级回退链各一级一测,外加
// href 转义野生态、无封面、坏 zip、封面非图片、大图降档。

import (
	"archive/zip"
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

const (
	epubContainerXML = `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`

	// EPUB3:properties 指定 cover-image
	epub3OPF = `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="uid">
  <manifest>
    <item id="c1" href="text/page1.jpg" media-type="image/jpeg"/>
    <item id="cover" href="cover.jpg" media-type="image/jpeg" properties="cover-image"/>
  </manifest>
</package>`

	// EPUB2:meta name="cover" 指向 manifest item id
	epub2OPF = `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="2.0" unique-identifier="uid">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>t</dc:title>
    <meta name="cover" content="cover-id"/>
  </metadata>
  <manifest>
    <item id="c1" href="text/page1.jpg" media-type="image/jpeg"/>
    <item id="cover-id" href="images/cover.jpeg" media-type="image/jpeg"/>
  </manifest>
</package>`
)

// testJPEG 造纯色不透明 JPEG(走编码管线的 JPEG 分支,尺寸可断言)
func testJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	red := color.RGBA{200, 30, 30, 255}
	for y := range h {
		for x := range w {
			img.Set(x, y, red)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// makeTestEPUB 把条目打包成 zip(即 epub 容器),返回路径
func makeTestEPUB(t *testing.T, entries map[string][]byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "book.epub")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, data := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestEpubCoverEPUB3Properties:EPUB3 按 properties="cover-image" 取,
// 不受条目顺序影响(page1 在 cover 之前)
func TestEpubCoverEPUB3Properties(t *testing.T) {
	p := makeTestEPUB(t, map[string][]byte{
		"mimetype":               []byte("application/epub+zip"),
		"META-INF/container.xml": []byte(epubContainerXML),
		"OEBPS/content.opf":      []byte(epub3OPF),
		"OEBPS/text/page1.jpg":   testJPEG(t, 77, 11), // 若误取首图会拿到 77x11
		"OEBPS/cover.jpg":        testJPEG(t, 40, 30),
	})
	td, err := MakeThumbnail(p)
	if err != nil {
		t.Fatal(err)
	}
	if td.W != 40 || td.H != 30 {
		t.Fatalf("封面取错:want 40x30 got %dx%d", td.W, td.H)
	}
	if td.Mime != "image/jpeg" {
		t.Fatalf("mime = %s", td.Mime)
	}
}

// TestEpubCoverEPUB2Meta:EPUB2 无 properties,靠 meta name="cover" 定位,
// 且 href 相对 OPF 目录(OEBPS/images/)解析
func TestEpubCoverEPUB2Meta(t *testing.T) {
	p := makeTestEPUB(t, map[string][]byte{
		"META-INF/container.xml":  []byte(epubContainerXML),
		"OEBPS/content.opf":       []byte(epub2OPF),
		"OEBPS/images/cover.jpeg": testJPEG(t, 50, 20),
	})
	td, err := MakeThumbnail(p)
	if err != nil {
		t.Fatal(err)
	}
	if td.W != 50 || td.H != 20 {
		t.Fatalf("封面取错:want 50x20 got %dx%d", td.W, td.H)
	}
}

// TestEpubCoverNameFallback:无任何元数据线索时按名回退 cover.*,
// 路径最浅者胜(根下 cover.jpg 压过深层 cover.png)
func TestEpubCoverNameFallback(t *testing.T) {
	p := makeTestEPUB(t, map[string][]byte{
		"META-INF/container.xml": []byte(epubContainerXML),
		"OEBPS/content.opf":      []byte(`<package version="3.0"><manifest><item id="a" href="a.jpg"/></manifest></package>`),
		"deep/dir/cover.png":     mustPNG(t, 9, 9),
		"cover.jpg":              testJPEG(t, 33, 44),
	})
	td, err := MakeThumbnail(p)
	if err != nil {
		t.Fatal(err)
	}
	if td.W != 33 || td.H != 44 {
		t.Fatalf("封面取错:want 33x44 got %dx%d", td.W, td.H)
	}
}

// TestEpubCoverHrefEscaped:规范要求 href 做 URL 转义,条目名是原文——
// 两种写法都应命中
func TestEpubCoverHrefEscaped(t *testing.T) {
	opf := `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <manifest><item id="cover" href="my%20cover.jpg" properties="cover-image"/></manifest>
</package>`
	p := makeTestEPUB(t, map[string][]byte{
		"META-INF/container.xml": []byte(epubContainerXML),
		"OEBPS/content.opf":      []byte(opf),
		"OEBPS/my cover.jpg":     testJPEG(t, 21, 12),
	})
	td, err := MakeThumbnail(p)
	if err != nil {
		t.Fatal(err)
	}
	if td.W != 21 || td.H != 12 {
		t.Fatalf("封面取错:want 21x12 got %dx%d", td.W, td.H)
	}
}

// TestEpubCoverNone:三级全落空 → errNoCover(预期,可 errors.Is 判定)
func TestEpubCoverNone(t *testing.T) {
	p := makeTestEPUB(t, map[string][]byte{
		"META-INF/container.xml": []byte(epubContainerXML),
		"OEBPS/content.opf":      []byte(`<package version="3.0"><manifest><item id="c1" href="c1.xhtml"/></manifest></package>`),
		"OEBPS/c1.xhtml":         []byte("<html/>"),
	})
	_, err := MakeThumbnail(p)
	if !errors.Is(err, errNoCover) {
		t.Fatalf("want errNoCover, got %v", err)
	}
}

// TestEpubCoverBrokenZip:连 zip 都不是 → 真实错误而非 errNoCover
// (上传侧只对 errNotImage/errNoCover 静默,此类要 Warn 留痕)
func TestEpubCoverBrokenZip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "broken.epub")
	if err := os.WriteFile(p, []byte("this is not a zip"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := MakeThumbnail(p)
	if err == nil || errors.Is(err, errNoCover) {
		t.Fatalf("want 非 errNoCover 的错误, got %v", err)
	}
}

// TestEpubCoverNotImage:元数据指到的条目不是图片 → errNotImage
func TestEpubCoverNotImage(t *testing.T) {
	opf := `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <manifest><item id="cover" href="cover.jpg" properties="cover-image"/></manifest>
</package>`
	p := makeTestEPUB(t, map[string][]byte{
		"META-INF/container.xml": []byte(epubContainerXML),
		"OEBPS/content.opf":      []byte(opf),
		"OEBPS/cover.jpg":        []byte("definitely not an image"),
	})
	_, err := MakeThumbnail(p)
	if !errors.Is(err, errNotImage) {
		t.Fatalf("want errNotImage, got %v", err)
	}
}

// TestEpubCoverDownscale:大封面走统一缩放(最长边 512)
func TestEpubCoverDownscale(t *testing.T) {
	opf := `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <manifest><item id="cover" href="cover.jpg" properties="cover-image"/></manifest>
</package>`
	p := makeTestEPUB(t, map[string][]byte{
		"META-INF/container.xml": []byte(epubContainerXML),
		"OEBPS/content.opf":      []byte(opf),
		"OEBPS/cover.jpg":        testJPEG(t, 1000, 600),
	})
	td, err := MakeThumbnail(p)
	if err != nil {
		t.Fatal(err)
	}
	if td.W != 512 || td.H != 307 {
		t.Fatalf("want 512x307(等比), got %dx%d", td.W, td.H)
	}
}

// mustPNG 造不透明 PNG(名称回退测 cover.png 用)
func mustPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{30, 90, 200, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
