# Makefile — 统一封装 wails 命令的 -tags webkit2_41(Arch 仓库已无 webkit2gtk-4.0,
# tag 恒必带;对 Windows 交叉编译目标无影响)。go 命令不需要该 tag。

TAGS := -tags webkit2_41

.PHONY: dev build cli test check fmt

dev: ## 开发模式:热重载打开窗口(前端 vite + Go 绑定)
	wails dev $(TAGS)

build: ## 发布构建 → build/bin/kist(前端 vue-tsc + vite 先行)
	wails build $(TAGS)

cli: ## CLI 构建 → build/bin/kistctl(kistctl-sandbox 指向这里)
	go build -o build/bin/kistctl ./cmd/kistctl

test: ## 全量测试(竞态检测)
	go test ./... -race -count=1

check: ## 阶段收尾验收:build + vet + gofmt + 全量测试
	go build ./... && go vet ./... && test -z "$$(gofmt -l .)" && $(MAKE) test

fmt: ## gofmt 全仓
	gofmt -w .

help: ## 列出目标
	@grep -h -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-8s\033[0m %s\n", $$1, $$2}'
