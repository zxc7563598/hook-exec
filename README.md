# HookExec

接收 GitHub webhook，按仓库触发服务器上预先放好的 shell 脚本。

一个二进制、一个 SQLite 文件、一个脚本目录，没有外部依赖，也不需要 Node.js 或任何外网资源。

```
GitHub ──webhook──▶ HookExec ──▶ <数据目录>/shell/deploy.sh
                       │
                       └──▶ 执行记录（SQLite）──▶ 管理页 /web
```

## 为什么有这个东西

手上有一堆博客、文档站和小服务，分散在不同仓库里。让每个服务自己监听自己的 webhook 并不是做不到，但每加一个站就要重复一遍「起个小 HTTP 服务 + 验签 + 解析 payload + 调部署命令」，而且这些逻辑散在各处，出没出问题得一个个去看。

HookExec 把这件事收敛成一个常驻服务：所有仓库的 webhook 都打到它这里，它按 `owner/repo` 找到对应的脚本执行。加一个新站就是「新建一个项目 + 传一个脚本」，不用再动服务本身。

## 安全模型：脚本不由这个项目创建

这是整个设计的前提。**网页端不能编写或修改脚本**，可执行内容全部来自 `<数据目录>/shell/`，由你自己通过 scp / ansible / git 传上去。由此带来的结果是：拿到 `/web` 的账号密码，只意味着**能从服务器上已有的脚本里挑一个执行**，而不是能执行任意命令。

脚本路径的解析收敛在 `internal/shelldir` 一处：拼出来的路径必须落在脚本目录内（软链解掉之后再判一次）、必须是普通文件，而且**每次执行前都重新校验**，不信任数据库里存的值 —— 即使有人直接改库、或导入一份恶意配置，也逃不出脚本目录。

**但 `/web` 的凭证仍然是服务器级机密**：持有者可以从脚本目录里任选脚本执行，也能读到它们的源码。请通过 HTTPS 反代暴露，不要直接裸奔在公网上。

---

## 快速开始

### 编译

```bash
git clone <repo> && cd hook-exec
make build          # 产出单个可执行文件 hook-exec（约 23MB）
```

需要 Go 1.25+。只有 `make css`（重新生成前端样式）才需要 Node.js，日常编译不需要。

### 运行

```bash
./hook-exec --listen 127.0.0.1:9000 --data-dir /opt/hookexec
```

首次启动会自动建好数据目录和 `shell/` 子目录。**没给密码时会随机生成一个并打印出来，只显示这一次**：

```
level=INFO msg="hook-exec 已启动" version=v1.0.0 listen=127.0.0.1:9000 data_dir=/opt/hookexec
level=INFO msg="脚本目录（把你的脚本放进来并 chmod +x）" path=/opt/hookexec/shell
level=INFO msg=管理页 url=http://127.0.0.1:9000/web
level=WARN msg=未指定密码，已生成随机密码。请立即保存，此密码只显示这一次 user=admin password=1c79a85843a6035bc3e19f74
level=INFO msg="GitHub webhook 地址" url=http://127.0.0.1:9000/
```

（日志走 slog 的 TextHandler，每行开头还有一个 `time=` 字段，这里省略了。）

跑起来之后：

1. 打开 `http://127.0.0.1:9000/web` 登录管理页
2. 把脚本放进 `<数据目录>/shell/` 并 `chmod +x`，点「重新扫描」
3. 新建项目，把仓库和脚本关联起来
4. 到 GitHub 仓库的 Settings → Webhooks → Add webhook 填上 `https://你的域名/`、Content type 选 `application/json`、Secret 填和项目里一致的那个

配好后 GitHub 会立刻发一个 `ping` 事件做连通性检查，HookExec 回 200 并且什么都不做。

### 数据目录

```
/opt/hookexec/
├── hook-exec                   ← 单个可执行文件
├── hook-exec.db                ← SQLite（含 WAL 附属文件）
└── shell/                      ← 脚本目录，本项目管理页只读
    ├── deploy.sh
    └── notify.py
```

默认取可执行文件所在目录。systemd 下二进制通常在 `/usr/local/bin`，这时用 `--data-dir /opt/hookexec` 指定数据位置。目录不可写时会直接报错退出，而不是等到写记录时才炸。

---

## 启动参数

| 参数 | 默认值 | 环境变量 | 说明 |
|---|---|---|---|
| `--listen` | `127.0.0.1:9000` | `HOOKEXEC_LISTEN` | 监听地址。默认只监听本机，要对外服务请走反代 |
| `--data-dir` | 二进制所在目录 | — | 数据库与 `shell/` 都放这里 |
| `--web-user` | `admin` | `HOOKEXEC_WEB_USER` | 管理页用户名 |
| `--web-pass` | 空 | `HOOKEXEC_WEB_PASS` | 管理页密码。**都没给就随机生成并打印一次** |
| `--concurrency` | `4` | — | 全局同时执行的脚本数上限 |
| `--retention-days` | `30` | — | 执行记录保留天数 |
| `--retention-per-project` | `500` | — | 每个项目保留的记录条数上限 |
| `--debug` | 关 | — | gin 非 release 模式 + debug 级日志（会记录每个 HTTP 请求） |

优先级：命令行参数 > 环境变量 > 默认值。密码支持环境变量是为了避免它出现在 `ps` 输出和 shell history 里，生产环境建议用这个方式传。

`--concurrency`、`--retention-days`、`--retention-per-project` 必须大于 0，`--web-user` 不能为空，否则启动即报错退出。不支持位置参数。

---

## 写一个脚本

脚本是普通的可执行文件，靠 shebang 决定解释器，**不需要**是 `sh` 脚本。**必须有可执行位**，否则不会被执行（页面上会标黄提醒）。

```bash
#!/bin/bash
set -euo pipefail

echo "收到 $HOOK_REPO 的 $HOOK_EVENT，分支 $HOOK_BRANCH"

cd /opt/myapp
git fetch --all
git reset --hard "$HOOK_SHA"
systemctl restart myapp

# 需要 payload 里的深层字段时直接 jq
jq -r '.commits[].message' "$HOOK_PAYLOAD_FILE"
```

上传：

```bash
scp deploy.sh server:/opt/hookexec/shell/
ssh server 'chmod +x /opt/hookexec/shell/deploy.sh'
```

脚本目录**只列第一层**，子目录不会被展开，也不能选。

> 建议脚本目录设成 `0755`，并且**不要给 hook-exec 的运行用户写权限**。
> 服务只读脚本，改脚本用另一个身份 —— 这是纵深防御。

### 注入的环境变量

| 变量 | 内容 |
|---|---|
| `HOOK_EXEC_ID` | 本次执行记录 ID |
| `HOOK_PROJECT` | 项目名称 |
| `HOOK_REPO` | `owner/repo` |
| `HOOK_EVENT` | `push` / `pull_request` / `release` … |
| `HOOK_DELIVERY` | GitHub 的 `X-GitHub-Delivery` |
| `HOOK_REF` | `refs/heads/main` |
| `HOOK_BRANCH` | `main` |
| `HOOK_SHA` | 提交 SHA |
| `HOOK_ACTOR` | 触发者 |
| `HOOK_SCRIPT` | 正在执行的脚本文件名 |
| `HOOK_TRIGGER` | `webhook` 或 `manual` |
| `HOOK_PAYLOAD_FILE` | 完整 payload JSON 的临时文件路径，执行结束即删 |

同时提供 GitHub Actions 风格的同义别名，现成的部署脚本改一下就能用：

`GITHUB_REPOSITORY`、`GITHUB_EVENT_NAME`、`GITHUB_SHA`、`GITHUB_REF`、`GITHUB_REF_NAME`、`GITHUB_ACTOR`、`GITHUB_DELIVERY`

**取不到的字段一律不注入**（而不是注入空串），所以脚本里可以用 `${HOOK_BRANCH:-main}` 区分「没有这个信息」和「信息为空」。不同事件能取到的字段不一样 —— `pull_request`、`release` 这类 payload 根本没有顶层 `ref`，这时 `HOOK_REF` / `HOOK_BRANCH` 会分别从 `pull_request.head.ref`、`release.tag_name` 里取。

**完整 payload 也会写到 stdin**，所以 `jq -r '.commits[].message'` 直接读 stdin 也能用，不必依赖临时文件。

### 工作目录与超时

- **工作目录**留空时继承进程当前目录。填了就在那个目录里执行；目录不存在会直接失败并在记录里给出提示。
- **超时**到达后会**终止整个进程组**，而不只是杀掉顶层进程 —— 脚本派生的 `npm run build`、后台起的服务都会被一起收掉，不会变成孤儿进程。先发 `SIGTERM`，宽限 3 秒后仍在跑才 `SIGKILL`。
- 每条执行记录**只保留输出的末尾 32KB**，页面上会标明被截断。

---

## 运行时行为

- **异步**：webhook 立刻返回 200，脚本在后台跑，结果写进数据库。管理页的执行记录每 2.5 秒自动刷新。
- **同项目串行，跨项目并行**：两次 `push` 连着到来时，部署脚本按顺序跑，不会在服务器上互相打架；不同项目并行，总量由 `--concurrency` 兜住。单个项目的排队上限是 64 条，超了就把该条标成「已跳过」，**webhook 依然返回 200**。
- **幂等**：GitHub 手动 redeliver 会重复投递同一个 delivery，重复投递不会重复执行。
- **重启**：正常停机（SIGINT / SIGTERM）会等在途脚本跑完（宽限 30 秒）；`kill -9` 之后重启，会把停在「排队中」「执行中」的记录收尾成「已中断」，不会留下看起来永远在跑的记录。

webhook 响应速查：

| 情况 | 响应 |
|---|---|
| 正常触发 | `200` `{"matched":1,"queued":1,…}` |
| 重复投递（redeliver） | `200` `{…,"duplicated":1}`，不重复执行 |
| 队列满 | `200` `{…,"skipped":1,"message":"队列已满…"}` |
| 仓库没配过、配了但停用、或签名不对 | 一律 `401` `{"message":"签名校验失败"}` |
| 事件不匹配 | `200` `{"matched":0,…}`，且不产生记录 |
| 服务正在停机 | `503`，让 GitHub 稍后重投 |

> 为什么那三种情况合并成同一个 `401`：如果「仓库没配过」回 200 而「签名不对」回 401，一个**不需要任何凭证**的请求就能靠状态码判断某个仓库在这台服务器上配了没有。响应完全一致就什么都问不出来。代价是给一个还没配置的仓库加 webhook 时，GitHub 那边会标红 —— 这其实也是有用的提示，具体原因会写在服务端日志里。

---

## 部署

### systemd

```ini
[Unit]
Description=HookExec
After=network.target

[Service]
Type=simple
User=hookexec
ExecStart=/opt/hookexec/hook-exec --data-dir /opt/hookexec
Environment=HOOKEXEC_WEB_PASS=<你的密码>
Restart=on-failure
RestartSec=5

# 加固：脚本目录只读，其他路径尽量不可写
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ReadWritePaths=/opt/hookexec
ReadOnlyPaths=/opt/hookexec/shell

[Install]
WantedBy=multi-user.target
```

### 反向代理

Basic Auth 在明文 HTTP 上会泄露凭证，生产环境请放在 TLS 反代后面：

```nginx
location / {
    proxy_pass http://127.0.0.1:9000;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-Proto $scheme;
}
```

### 备份

需要备份的只有两样：`hook-exec.db`（配置 + 执行记录）和 `shell/`（脚本）。管理页的「导出配置」也能导出一份 JSON，**里面含明文密钥**，请妥善保管。

---

## 打包发布

```bash
git tag v1.0.0
make release
```

产物落在 `dist/`：四个平台（linux/darwin × amd64/arm64）的 `.tar.gz` 加一份 `checksums.txt`，每个包里是一个 `hook-exec` 和这份 README，解压即用。

交叉编译靠 `CGO_ENABLED=0` —— SQLite 走纯 Go 实现，不需要目标平台的工具链，一台机器就能打出全部四个包。版本号通过 `-ldflags` 注入，启动日志第一行会打出来。

**版本号取自 HEAD 上的 tag**，没有 tag 或工作区有未提交改动时会直接拒绝，免得打出一个对不上任何一次提交的包。试打和裁剪平台：

```bash
make release RELEASE_VERSION=v1.0.0-rc1 ALLOW_DIRTY=1
make release PLATFORMS="linux/amd64 linux/arm64"
```

**没有 Windows**：进程组管理（`Setpgid`、`kill(-pgid)`）是 Unix 专有的，而「超时终止整个进程组」是这里明确的行为保证；可执行位在 Windows 上也没有意义。

**macOS 下载后跑不起来**：从浏览器下载的包会被打上隔离属性，首次运行可能被 Gatekeeper 拦下，`xattr -d com.apple.quarantine hook-exec` 解掉即可。`curl` 下载的不会带这个属性。

---

## 开发

```bash
make run     # 本地起来跑，数据放 ./devdata，debug 模式
make vet     # go vet
make build   # 编译
```

> 仓库当前没有单元测试文件（`make test` 会跑 `go test -race ./...`，但会报 `no test files`）。

### 前端

页面在 `internal/webui/index.html`，单文件，Vue 3 + Tailwind，**没有构建步骤**。Vue 和预编译好的 Tailwind CSS 都在 `internal/webui/vendor/` 下随二进制一起分发，部署机器不需要 Node.js，也不依赖任何外网 CDN。

> ⚠️ `vendor/app.css` 是**构建产物，且已提交进仓库**。如果你在 `index.html` 里加了新的 Tailwind 类名，必须跑一次 `make css`（用 `npx tailwindcss`）重新生成，否则新类名在产物里不存在、样式会静默失效。
>
> 类名必须写成完整字面量（不能拼 `'bg-' + color`），Tailwind 是在构建时按文本扫描的。

### 管理接口

管理页用的接口都在 `/web/api/` 下，整组受 Basic Auth 保护，**写操作额外要求 `X-Requested-With: XMLHttpRequest` 头**（Basic Auth 凭证会被浏览器自动缓存并附加到跨站请求上，光有密码挡不住跨站写操作，所以加了这道同源检查）。

`projects`（含 `/:id/enabled`、`/:id/run`）、`scripts`、`scripts/content`、`executions`、`config/export`、`config/import`。响应统一是 `{"code":…,"message":…,"data":…}`，`code` 与 HTTP 状态码始终一致。分页 `page_size` 默认 20、上限 200；清空记录必须带 `project_id` 或 `status` 筛选，防止误清全表。

### 目录结构

```
main.go                      装配与优雅停机
internal/config/             启动参数、数据目录定位
internal/shelldir/           脚本目录的路径校验（核心安全控制）
internal/model/              实体与事件匹配规则
internal/store/              SQLite 读写、保留清理、启动对账
internal/webhook/            签名校验、payload 解析、webhook 处理
internal/executor/           并发模型、进程组管理、输出采集
internal/api/                管理接口与 Basic Auth
internal/webui/              单页与内嵌静态资源
```

## 许可证

MIT
