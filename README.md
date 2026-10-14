# HookExec

接收 GitHub webhook，执行服务器上预先放好的脚本。

给运维用的小工具：一个二进制、一个 SQLite 文件、一个脚本目录，没有外部依赖。

```
GitHub ──webhook──▶ HookExec ──▶ /opt/hookexec/shell/deploy.sh
                       │
                       └──▶ 执行记录（SQLite）──▶ 管理页 /web
```

整个服务只暴露两个路由：

| 路由 | 用途 |
|---|---|
| `POST /` | 接收 GitHub webhook，靠 HMAC 签名校验，不需要认证 |
| `GET /` | 返回 `hook-exec`，可以当存活探针用，不泄露任何信息 |
| `ANY /web/*` | 管理页与管理接口，全部受 Basic Auth 保护 |

---

## 安全模型：脚本不由这个项目创建

这点是刻意的，也是它和多数同类工具最大的不同：

**网页端不能编写或修改脚本。** 可执行内容全部来自 `<数据目录>/shell/` 目录，
由你自己通过 scp / ansible / git 上传到服务器。HookExec 只从那个目录里读取和执行，
并且强制校验路径不能逃逸出去。

带来的结果是：

- 拿到 `/web` 的账号密码，只意味着**能从服务器上已有的脚本里挑一个执行**，
  而不是能执行任意命令。
- 要真正让服务器跑点新东西，仍然需要 shell 或文件系统权限去上传脚本。

**信任边界：** `/web` 的凭证仍是服务器级机密 —— 持有者可以从脚本目录里任选脚本执行，
也能读取它们的源码。请通过 HTTPS 反代暴露，不要直接裸奔在公网上。

### 路径校验

`internal/shelldir` 是唯一允许接触脚本文件的入口，四道校验全过才执行：

1. 形状检查 —— 拒绝空名、绝对路径、`.`、`..`
2. 拼接后必须落在脚本目录内（挡住 `../` 的各种变体）
3. 解掉符号链接后再判断一次落点（挡住 `shell/evil.sh -> /etc/passwd`）
4. 必须是普通文件（挡住目录、FIFO、设备文件）

而且**每次执行前都会重新校验一遍**，不信任数据库里存的值 —— 即使有人直接改库、
或者导入了一份恶意配置，也逃不出脚本目录。

---

## 快速开始

```bash
# 编译
make build          # 产出单个可执行文件 hook-exec（约 23MB）

# 运行（未指定密码时会生成一个随机密码并打印出来）
./hook-exec --listen 127.0.0.1:9000

# 首次启动会自动建好目录结构
```

启动后会打印：

```
INFO  脚本目录（把你的脚本放进来并 chmod +x） path=/opt/hookexec/shell
INFO  管理页 url=http://127.0.0.1:9000/web
WARN  未指定密码，已生成随机密码。请立即保存，此密码只显示这一次
      user=admin password=<随机密码>
INFO  GitHub webhook 地址 url=http://127.0.0.1:9000/
```

### 运行时目录布局

```
/opt/hookexec/                  ← 二进制所在目录，可用 --data-dir 覆盖
├── hook-exec                   ← 单个可执行文件
├── hook-exec.db                ← SQLite（含 WAL 附属文件）
└── shell/                      ← 脚本目录，本项目管理页碰不到
    ├── deploy.sh
    └── notify.py
```

默认取二进制所在目录。systemd 下二进制通常在 `/usr/local/bin`，
这时用 `--data-dir /opt/hookexec` 指定数据位置。数据目录不可写时会直接报错退出。

---

## 写一个脚本

脚本是普通的可执行文件，靠 shebang 决定解释器，**不需要** `#!/bin/sh`。
需要可执行位，否则不会被执行（页面上会标黄提醒）。

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

上传流程：

```bash
scp deploy.sh server:/opt/hookexec/shell/
ssh server 'chmod +x /opt/hookexec/shell/deploy.sh'
```

然后在管理页点「重新扫描」，下拉框里就会出现它。

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
| `HOOK_REF` | `refs/heads/main`（能取到才注入） |
| `HOOK_BRANCH` | `main` |
| `HOOK_SHA` | 提交 SHA（能取到才注入） |
| `HOOK_ACTOR` | 触发者 |
| `HOOK_SCRIPT` | 正在执行的脚本文件名 |
| `HOOK_TRIGGER` | `webhook` 或 `manual` |
| `HOOK_PAYLOAD_FILE` | 完整 payload JSON 的临时文件路径，执行结束即删 |

同时还提供 GitHub Actions 风格的同义别名：`GITHUB_REPOSITORY`、`GITHUB_EVENT_NAME`、
`GITHUB_SHA`、`GITHUB_REF_NAME`、`GITHUB_REF`、`GITHUB_ACTOR`。

**完整 payload 也会写到 stdin**，所以 `cat | jq ...` 也能用，不必依赖临时文件。
取不到的字段一律不注入，脚本里可以用 `${HOOK_BRANCH:-main}` 给默认值。

### 工作目录与超时

- **工作目录**留空时继承进程当前目录。填了就在那个目录里执行（目录不存在会直接失败并给出提示）。
- **超时**到达后会**终止整个进程组**，而不只是杀掉顶层进程 —— 脚本派生的
  `npm run build`、后台起的服务都会被一起收掉，不会变成孤儿进程。

---

## 配置项目

在管理页「新建项目」，填这些：

| 字段 | 说明 |
|---|---|
| 项目名称 | 只是给你自己看的标签 |
| 仓库 | `owner/repo`，必须和 payload 里的 `repository.full_name` 完全一致 |
| Webhook 密钥 | 和 GitHub webhook 设置页里填的 secret 一致，用于校验签名。可点「生成」随机生成一个 |
| 脚本文件 | 从脚本目录里下拉选择 |
| 事件类型 | 多选，也可勾「全部事件」。一个都不选等同于全部事件 |
| 工作目录 | 可选 |
| 超时时间 | 秒，默认 300 |
| 启用 | 停用后 webhook 仍返回成功，但不执行 |

卡片上的开关是即时生效的，走单字段接口，不会因为表单里别的字段没填对而调不动。

### 在 GitHub 上配置 webhook

仓库 → Settings → Webhooks → Add webhook：

- **Payload URL**：`https://your-host/`
- **Content type**：`application/json`
- **Secret**：和项目里填的一致
- **事件**：按需勾选

配好后 GitHub 会立刻发一个 `ping` 事件，HookExec 会回 200 并什么都不做。

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

密码优先从环境变量读，避免出现在 `ps` 输出和 shell history 里。

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

需要备份的只有两样：`hook-exec.db`（配置 + 执行记录）和 `shell/` 目录（脚本）。
管理页的「导出配置」也能导出一份 JSON，注意**里面含明文密钥**。

---

## 启动参数

| 参数 | 默认值 | 说明 |
|---|---|---|
| `--listen` | `127.0.0.1:9000` | 监听地址。默认只监听本机，要对外服务请走反代 |
| `--data-dir` | 二进制所在目录 | 数据库与 `shell/` 都放这里 |
| `--web-user` | `admin` | 管理页用户名 |
| `--web-pass` | 空 | 管理页密码。也可用 `HOOKEXEC_WEB_PASS`。**都没给就随机生成并打印一次** |
| `--concurrency` | `4` | 全局同时执行的脚本数上限 |
| `--retention-days` | `30` | 执行记录保留天数 |
| `--retention-per-project` | `500` | 每个项目保留的记录条数上限 |
| `--debug` | 关 | gin debug 模式 + debug 级日志（会记录每个 HTTP 请求） |

对应环境变量：`HOOKEXEC_LISTEN`、`HOOKEXEC_WEB_USER`、`HOOKEXEC_WEB_PASS`。

---

## 行为细节

### 执行是异步的

webhook 收到后会立刻返回 200（通常 1–2ms），脚本在后台跑。执行结果写进数据库，
在管理页的「执行记录」里看，页面在打开记录视图时每 2.5 秒自动刷新。

队列满了不会阻塞 webhook，而是把该条记录标成「已跳过」，仍然返回 200。

### 同一个项目串行，不同项目并行

每个项目有自己的一条执行队列，队列里的任务按顺序依次执行。

这一点很重要：两次 `push` 连着到来时，部署脚本必须按顺序跑，不能在服务器上互相打架。
不同项目之间并行，总量由 `--concurrency` 兜住。

### 幂等

GitHub 支持手动 redeliver，会重复投递同一个 `X-GitHub-Delivery`。
重复投递不会重复执行，webhook 响应里会显示 `duplicated: 1`。

### 各种情况下返回什么

| 情况 | 响应 |
|---|---|
| 正常触发 | `200` `{"matched":1,"queued":1}` |
| 重复投递（redeliver） | `200` `{"matched":1,"duplicated":1}`，不重复执行 |
| 仓库没配过、配了但停用、或签名不对 | 一律 `401` `{"message":"签名校验失败"}` |
| 事件不匹配 | `200` `{"matched":0}`，且不产生记录 |
| `ping` 事件 | `200` `{"message":"pong"}` |
| 服务正在停机 | `503`，让 GitHub 稍后重投 |
| payload 超过 5MB | `413` |

> 关于那三种情况为什么合并成同一个 `401`：如果"仓库没配过"回 200 而"签名不对"回 401，
> 一个**不需要任何凭证**的请求就能靠状态码判断某个仓库在这台服务器上配了没有，
> 等于把配置信息枚举出去。响应完全一致就什么都问不出来。
> 代价是给一个还没在 HookExec 里配置的仓库加 webhook 时，GitHub 那边会标红 ——
> 这其实也是有用的提示，具体原因会写在服务端日志里。

### 重启与崩溃

- 正常停机（SIGTERM）时会先停止收新请求，等在途脚本跑完（宽限 30 秒），
  超时才强制终止进程组。在途记录标为「已中断」。
- 进程被 `kill -9` 后重启时，会把停在「排队中」「执行中」的记录收尾成「已中断」，
  不会留下看起来永远在跑的记录。

### 记录清理

后台每小时按两条约束清理（取更严格者）：早于 `--retention-days` 的记录，
以及每个项目超出 `--retention-per-project` 条的更早部分。

---

## 开发

```bash
make run     # 本地起来，数据放 ./devdata，debug 模式
make test    # 单元测试（含 -race）
make vet     # go vet
make build   # 编译
```

### 前端

页面在 `internal/webui/index.html`，单文件，Vue 3 + Tailwind，**没有构建步骤**。
Vue 和预编译好的 Tailwind CSS 都在 `internal/webui/vendor/` 下随二进制一起分发，
部署机器不需要 Node.js，也不依赖任何外网 CDN。

> ⚠️ `vendor/app.css` 是**构建产物**。如果你在 `index.html` 里加了新的 Tailwind 类名，
> 必须跑一次 `make css`（用 `npx tailwindcss`）重新生成，否则新类名没有样式。
> 类名必须写成完整字面量（不能拼 `'bg-' + color`），Tailwind 是在构建时扫描文本的。

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

---

## 发布

```bash
git tag v0.1.0
make release
```

产物落在 `dist/`：

```
dist/
├── hook-exec_v0.1.0_linux_amd64.tar.gz
├── hook-exec_v0.1.0_linux_arm64.tar.gz
├── hook-exec_v0.1.0_darwin_amd64.tar.gz
├── hook-exec_v0.1.0_darwin_arm64.tar.gz
└── checksums.txt
```

每个包里是一个 `hook-exec` 可执行文件和这份 README，解压即用。

交叉编译靠 `CGO_ENABLED=0` —— SQLite 走纯 Go 实现，所以不需要目标平台的
工具链，在一台机器上就能打出全部四个包。版本号通过 `-ldflags` 注入，
启动日志第一行会打出来。

**版本号取自 HEAD 上的 tag。** 没有 tag，或者工作区有未提交改动时，
`make release` 会直接拒绝 —— 免得打出一个对不上任何一次提交、却带着
正式版本号的包。试打时可以用：

```bash
make release RELEASE_VERSION=v0.1.0-rc1 ALLOW_DIRTY=1
```

平台列表可以覆盖：

```bash
make release PLATFORMS="linux/amd64 linux/arm64"
```

### 为什么没有 Windows

不是偷懒，是这套代码在 Windows 上跑不起来，硬编也编不过：

- 进程组的建立与终止（`Setpgid`、`kill(-pgid)`）是 Unix 专有的，
  而"超时终止整个进程组"是这里明确的行为保证，砍掉它脚本派生的
  `npm run build` 会变成孤儿进程继续跑。
- 可执行位在 Windows 上没有意义（`Perm()` 恒为 `0666` 或 `0444`），
  那套"脚本没有可执行位就提前报错"的检查会误杀掉全部脚本。

真要支持得另写一套 Job Object 的进程树管理，以及一套 Windows 的
脚本可执行性判断。

### macOS 下载后跑不起来

从浏览器下载的包会被打上隔离属性，首次运行可能被 Gatekeeper 拦下：

```bash
xattr -d com.apple.quarantine hook-exec
```

`curl` 下载的不会带这个属性。

---

## 许可证

MIT
