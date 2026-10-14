# HookExec 构建入口
#
# 日常只需要 make build。css 目标只在改了 index.html 里的 Tailwind 类名
# 之后才需要跑一次 —— vendor/app.css 是构建产物，已经提交进仓库，
# 部署机器上不需要 Node.js。

BINARY     := hook-exec
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS    := -s -w -X main.version=$(VERSION)
WEBUI      := internal/webui

.PHONY: all build css run test vet fmt clean tidy help

all: build

## build: 编译单个可执行文件（含内嵌前端资产）
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) .
	@echo "已生成 $(BINARY)  $$(du -h $(BINARY) | cut -f1)"

## css: 用 Tailwind CLI 重新生成 vendor/app.css
##      改了 index.html 里的类名之后必须跑一次，否则新类名没有样式
css:
	cd $(WEBUI) && npx --yes tailwindcss@3.4.17 -c tailwind.config.js -i input.css -o vendor/app.css --minify

## run: 本地起来跑一下，数据放在 ./devdata，不污染仓库根目录
run:
	go run . --data-dir ./devdata --debug

## test: 跑单元测试（含竞态检测）
test:
	go test -race ./...

## vet: 静态检查
vet:
	go vet ./...

## fmt: 格式化
fmt:
	gofmt -w .

## tidy: 整理依赖
tidy:
	go mod tidy

## clean: 清理构建产物（不动 devdata 和数据库）
clean:
	rm -f $(BINARY)

## help: 列出可用目标
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'
