# HookExec 构建入口
#
# 日常只需要 make build。css 目标只在改了 index.html 里的 Tailwind 类名
# 之后才需要跑一次 —— vendor/app.css 是构建产物，已经提交进仓库，
# 部署机器上不需要 Node.js。

BINARY     := hook-exec
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS    := -s -w -X main.version=$(VERSION)
WEBUI      := internal/webui

# 发布相关。目标平台只列 Linux 与 macOS：Windows 上进程组管理那套
# （Setpgid / kill(-pgid)）和可执行位语义都不成立，代码本身就跑不起来。
DIST       := dist
PLATFORMS  ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

# 发布版本号取自 HEAD 上的 tag，保证打出来的包对得上一次提交。
# 也可以显式指定：make release RELEASE_VERSION=v0.1.0
RELEASE_VERSION ?= $(shell git describe --tags --exact-match 2>/dev/null)
RELEASE_LDFLAGS := -s -w -X main.version=$(RELEASE_VERSION)

# sha256 的写法在 GNU coreutils 和 BSD 上不一样，两边都认一下
SHA256SUM  := $(shell command -v sha256sum 2>/dev/null || command -v shasum 2>/dev/null)
SHA_FLAG   := $(if $(findstring sha256sum,$(SHA256SUM)),,-a 256)

.PHONY: all build css run test vet fmt clean tidy release help

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
	rm -rf $(DIST)

## release: 交叉编译出各平台发布包（tar.gz + sha256），放进 dist/
##          版本号取自 HEAD 上的 tag，可用 RELEASE_VERSION=v0.1.0 覆盖
##          平台列表可用 PLATFORMS="linux/amd64 ..." 覆盖
##          工作区有未提交改动时会拒绝构建，ALLOW_DIRTY=1 可跳过该检查
release:
	@if [ -z "$(RELEASE_VERSION)" ]; then \
		echo "错误：HEAD 上没有 tag，无法确定发布版本号。"; \
		echo "      先打 tag（git tag v0.1.0 && git push --tags），"; \
		echo "      或显式指定版本：make release RELEASE_VERSION=v0.1.0"; \
		exit 1; \
	fi
	@if [ -n "$$(git status --porcelain)" ] && [ -z "$(ALLOW_DIRTY)" ]; then \
		echo "错误：工作区有未提交改动，打出来的包对应不上任何一次提交。"; \
		echo "      先提交，或用 ALLOW_DIRTY=1 跳过检查（仅用于试打）。"; \
		exit 1; \
	fi
	@rm -rf $(DIST) && mkdir -p $(DIST)
	@echo "版本 $(RELEASE_VERSION)，开始交叉编译："
	@for p in $(PLATFORMS); do \
		os=$${p%%/*}; arch=$${p##*/}; \
		name=$(BINARY)_$(RELEASE_VERSION)_$${os}_$${arch}; \
		mkdir -p "$(DIST)/$$name" || exit 1; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build -trimpath -ldflags "$(RELEASE_LDFLAGS)" \
			-o "$(DIST)/$$name/$(BINARY)" . || exit 1; \
		cp README.md "$(DIST)/$$name/" || exit 1; \
		tar -czf "$(DIST)/$$name.tar.gz" -C "$(DIST)" "$$name" || exit 1; \
		rm -rf "$(DIST)/$$name" || exit 1; \
		printf '  %-14s → dist/%s.tar.gz  %s\n' "$$os/$$arch" "$$name" \
			"$$(du -h "$(DIST)/$$name.tar.gz" | cut -f1)"; \
	done
	@if [ -n "$(SHA256SUM)" ]; then \
		cd $(DIST) && $(SHA256SUM) $(SHA_FLAG) *.tar.gz > checksums.txt; \
	else \
		echo "警告：没找到 sha256sum/shasum，未生成 checksums.txt"; \
	fi
	@echo "产物在 $(DIST)/："
	@ls -1 $(DIST)

## help: 列出可用目标
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'
