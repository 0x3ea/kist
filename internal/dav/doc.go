// Package dav 封装通用 WebDAV 客户端层。
//
// 职责:整文件 PUT(带 Content-Length,避免 chunked 传输被保守网盘拒绝)、流式 GET、
// 建目录/列表、删除/移动,以及统一的退避重试(网络错误/5xx/429)。
// 网络可靠性全部封装在本层,上层不感知 HTTP 细节。
// 设计文档:docs/phase-2-webdav.md
package dav
