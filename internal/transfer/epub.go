package transfer

// epub.go — epub 封面抽取:epub 是 zip 容器,封面按三级查找——
//  1. EPUB3:OPF manifest 中 properties 含 "cover-image" 的 item;
//  2. EPUB2:OPF <meta name="cover" content="id"> 指向的 item;
//  3. 名称回退:zip 内 basename 为 cover.* 的图片(路径最浅者胜,同深取词法序)。
//
// OPF 里的 href 相对 OPF 所在目录解析(OEBPS/content.opf 旁的 cover.jpg 在包内
// 是 OEBPS/cover.jpg);href 同时容忍 URL 转义与未转义两种野生态。
// container.xml 缺损/OPF 解析失败不致命,继续走名称回退;全部落空返回
// errNoCover——与 errNotImage 同级"属预期",上传侧静默跳过,只有 zip 本身
// 打不开才作为真实错误交由 Warn 留痕。纯标准库(archive/zip + encoding/xml),
// 依赖零新增。

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"slices"
	"strings"
)

// errNoCover 标记"epub 内没有可用封面":预期情形,上传侧与 errNotImage 同待
var errNoCover = errors.New("epub: 未找到封面")

// epubCoverMaxBytes 封面字节读取上限:防 zip 条目声明与实际不符的解压炸弹。
// 64MB 远超任何合理封面,超限即放弃——自动封面是尽力而为的派生缓存,不值得较真
const epubCoverMaxBytes = 64 << 20

// epubThumbnail 抽出封面字节后走统一的嗅探→缩放→编码管线
func epubThumbnail(srcPath string) (ThumbData, error) {
	data, err := epubCoverBytes(srcPath)
	if err != nil {
		return ThumbData{}, err
	}
	img, err := decodeImage(data, bytes.NewReader(data))
	if err != nil {
		return ThumbData{}, err
	}
	return encodeThumb(fitEdge(img, thumbMaxEdge), thumbMaxBytes)
}

// epubCoverBytes 按三级回退抽出封面字节;只有 zip 打不开才是硬错误
func epubCoverBytes(srcPath string) ([]byte, error) {
	zr, err := zip.OpenReader(srcPath)
	if err != nil {
		return nil, fmt.Errorf("epub: %w", err)
	}
	defer zr.Close()

	// 第 1/2 级:container.xml → OPF → 封面 href
	if opf := rootfilePath(&zr.Reader); opf != "" {
		if href := coverHrefFromOPF(&zr.Reader, opf); href != "" {
			if data, err := readEntryLoose(&zr.Reader, path.Dir(opf), href); err == nil {
				return data, nil
			}
			// href 悬空(条目缺失):不算失败,继续名称回退
		}
	}

	// 第 3 级:zip 内名为 cover.* 的图片
	best := ""
	for _, f := range zr.File {
		base := strings.ToLower(path.Base(f.Name))
		if strings.TrimSuffix(base, path.Ext(base)) != "cover" || !isImagePath(f.Name) {
			continue
		}
		if best == "" || strings.Count(f.Name, "/") < strings.Count(best, "/") ||
			(strings.Count(f.Name, "/") == strings.Count(best, "/") && f.Name < best) {
			best = f.Name
		}
	}
	if best != "" {
		if data, err := readEntryLoose(&zr.Reader, "", best); err == nil {
			return data, nil
		}
	}
	return nil, errNoCover
}

// rootfilePath 读 META-INF/container.xml 取 OPF 路径;缺失/坏格式返回 ""
func rootfilePath(r *zip.Reader) string {
	for _, f := range r.File {
		if !strings.EqualFold(f.Name, "META-INF/container.xml") {
			continue
		}
		data, err := readZipEntry(f, epubCoverMaxBytes)
		if err != nil {
			return ""
		}
		root := ""
		walkXML(data, func(el xml.StartElement) {
			if el.Name.Local == "rootfile" && root == "" {
				root = attrVal(el, "full-path")
			}
		})
		return root
	}
	return ""
}

// coverHrefFromOPF 解析 OPF 找封面 href:EPUB3 优先(properties 含
// cover-image),回落 EPUB2 的 meta name="cover" 指向项。只认元素/属性的
// local name——各版本的命名空间 URI 与前缀不一,前缀匹配是脆弱写法
func coverHrefFromOPF(r *zip.Reader, opfPath string) string {
	data, err := readEntryLoose(r, "", opfPath)
	if err != nil {
		return ""
	}
	items := map[string]string{} // id → href
	coverID := ""                // EPUB2:meta name="cover" 的 content
	epub3Href := ""              // EPUB3:properties 含 cover-image
	walkXML(data, func(el xml.StartElement) {
		switch el.Name.Local {
		case "item":
			id, href := attrVal(el, "id"), attrVal(el, "href")
			if id == "" || href == "" {
				return
			}
			if _, dup := items[id]; !dup {
				items[id] = href
			}
			if epub3Href == "" && slices.Contains(strings.Fields(attrVal(el, "properties")), "cover-image") {
				epub3Href = href
			}
		case "meta":
			if coverID == "" && attrVal(el, "name") == "cover" {
				coverID = attrVal(el, "content")
			}
		}
	})
	if epub3Href != "" {
		return epub3Href
	}
	return items[coverID] // coverID 为空时查不到,返回 ""
}

// walkXML 宽容地遍历 XML 每个开始元素:坏字节/非 XML 静默返回——解析失败
// 由调用方走名称回退,不在此报错
func walkXML(data []byte, fn func(xml.StartElement)) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	for {
		tok, err := dec.Token()
		if err != nil {
			return
		}
		if el, ok := tok.(xml.StartElement); ok {
			fn(el)
		}
	}
}

func attrVal(el xml.StartElement, name string) string {
	for _, a := range el.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// readZipEntry 读单个条目,限长防解压炸弹
func readZipEntry(f *zip.File, limit int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("epub: 条目 %s 超过 %d 字节上限", f.Name, limit)
	}
	return data, nil
}

// readEntryLoose 按名取条目字节,候选依次尝试:相对 OPF 目录解析后的路径、
// 原样路径、URL 解转义(规范要求 href 转义,野生态常不转义)、各自行内
// Clean。首个命中的条目胜出
func readEntryLoose(r *zip.Reader, baseDir, name string) ([]byte, error) {
	un, _ := url.PathUnescape(name)
	bases := []string{name, un}
	seen := map[string]bool{}
	for _, b := range bases {
		if b == "" {
			continue
		}
		for _, c := range []string{path.Join(baseDir, b), path.Clean(b)} {
			if seen[c] {
				continue
			}
			seen[c] = true
			for _, f := range r.File {
				if f.Name == c {
					return readZipEntry(f, epubCoverMaxBytes)
				}
			}
		}
	}
	return nil, fmt.Errorf("epub: 条目 %q 不存在", name)
}
