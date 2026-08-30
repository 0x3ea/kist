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
		// 显式上限绕过 wails#2431:v2 的 Linux 前端无条件调
		// gtk_window_set_geometry_hints(GDK_HINT_MAX_SIZE),未配置时拿
		// "显示器尺寸 + 装饰补偿差值"当上限,在原生 Wayland(实测 KDE)下
		// 差值算错 → 窗口被幻影上限卡死:最大化被 WM 拒绝、拖边不可缩放。
		// 显式给一个远超任何显示器的值后上限形同虚设,两处恢复正常;
		// 若日后升 v3(PR #4047 已修)可移除。
		MaxWidth:  16384,
		MaxHeight: 16384,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 27, G: 38, B: 54, A: 1},
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		// 在途传输时关窗要确认:传输 goroutine 随进程终止,临时产物虽会由
		// 下次启动清理,但半途而废的上传对用户是意外行为,值得一次询问。
		// (返回值 prevent 语义,详见 beforeClose 注释)
		OnBeforeClose: app.beforeClose,
		Bind: []interface{}{
			app,
		},
	})
	if err != nil {
		println("Error:", err.Error())
	}
}

// beforeClose 的返回值是 prevent 语义(Wails v2 options 签名:
// OnBeforeClose func(ctx) (prevent bool))——true = 阻止关闭。
// 曾把 true 当"允许关"写反,导致 SIGTERM 与点 X 都关不掉窗口、主线程
// 永挂 gtk_main(goroutine dump 定位)。
// 注意:Linux(GTK)前端忽略自定义 Buttons,QuestionDialog 固定 Yes/No,
// 返回值恒为英文 "Yes"/"No"(按钮显示文本随系统语言,判断不受影响)。
func (a *App) beforeClose(ctx context.Context) bool {
	if a.mgr == nil || a.mgr.Idle() {
		return false // 无在途传输,放行
	}
	dialog, err := wruntime.MessageDialog(ctx, wruntime.MessageDialogOptions{
		Type:    wruntime.QuestionDialog,
		Title:   "有传输正在进行",
		Message: "关闭窗口将中止进行中的上传/下载(临时文件由下次启动清理)。\n确定要关闭吗?",
	})
	if err != nil {
		return false // 对话框失败不拦人:宁可中止也别关不掉
	}
	return dialog != "Yes" // Yes = 确认中止并关闭(放行);其余一律阻止
}
