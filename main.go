package main

// main.go — Wails GUI 入口。窗口骨架沿用 Phase 6 合入的模板,Phase 7 增补:
// OnBeforeClose(在途传输确认)与 OnShutdown(防抖停表 + 退出前索引备份 + 关库)。

import (
	"context"
	"embed"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	app := NewApp()

	err := wails.Run(&options.App{
		Title:  "kist",
		Width:  1024,
		Height: 768,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 27, G: 38, B: 54, A: 1},
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		// 在途传输时关窗要确认:传输 goroutine 随进程终止,临时产物虽会由
		// 下次启动清理,但半途而废的上传对用户是意外行为,值得一次询问。
		OnBeforeClose: app.beforeClose,
		Bind: []interface{}{
			app,
		},
	})
	if err != nil {
		println("Error:", err.Error())
	}
}

// beforeClose 返回 false 阻止关窗;有在途传输时弹确认对话框。
// 注意:Linux(GTK)前端忽略自定义 Buttons,QuestionDialog 固定 Yes/No,
// 返回值恒为英文 "Yes"/"No"(按钮显示文本随系统语言,判断不受影响)。
func (a *App) beforeClose(ctx context.Context) bool {
	if a.mgr == nil || a.mgr.Idle() {
		return true
	}
	dialog, err := wruntime.MessageDialog(ctx, wruntime.MessageDialogOptions{
		Type:    wruntime.QuestionDialog,
		Title:   "有传输正在进行",
		Message: "关闭窗口将中止进行中的上传/下载(临时文件由下次启动清理)。\n确定要关闭吗?",
	})
	if err != nil {
		return true // 对话框失败不拦人:宁可中止也别关不掉
	}
	return dialog == "Yes"
}
