// Package index 实现明文索引(SQLite)。
//
// 索引是虚拟目录结构的唯一真源:网盘上只有随机名 blob,文件名、层级、blob 对应关系
// 全部记录在此;同时承载用户自定义数据(加密时间、备注、缩略图)与 LWW 所需的 revision。
// 设计文档:docs/phase-3-index.md
package index
