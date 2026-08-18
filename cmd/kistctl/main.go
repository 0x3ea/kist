// kistctl 是 kist 的命令行入口,与 GUI 共享全部核心逻辑(internal/ 下各包)。
//
// 子命令在 Phase 4 实现(config/init/unlock/put/ls/search/get/info/note/rm/gc/backup/pull),
// 规格见 docs/phase-4-pipeline-cli.md。
package main

import "fmt"

func main() {
	// Phase 0 仅验证骨架可构建;真实子命令在 Phase 4 落地
	fmt.Println("kistctl 骨架就绪(Phase 0);子命令将在 Phase 4 实现,见 docs/phase-4-pipeline-cli.md")
}
