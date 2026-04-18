# ocgo

一个很小的部署工具，用来绕开 OpenCodeUI 纯前端 Docker 部署时“更新要停容器、pull、再重启”的麻烦。

它不是上游项目本身，主要目的是把上游前端直接编译后嵌入到一个 Go 二进制里，部署和更新都只处理一个文件。

上游项目：<https://github.com/lehhair/OpenCodeUI>

上游仓库已经提供桌面/客户端产物；这个仓库只关注服务器上的纯前端远程访问场景，所以这里只发布 Linux 二进制。

## 设计

- 前端静态资源编译后直接嵌入二进制
- 工具启动后提供静态文件服务
- `/api/*` 反代到你的 `opencode serve`
- 每次更新只替换工具二进制，不再单独下载前端包
- 本仓库 release 跟随上游 release，同版本号发布

## 使用

下载对应平台的二进制（例如 release 中的 `ocgo-linux-amd64`）后：

```bash
./ocgo start
./ocgo start --foreground
./ocgo start --external
./ocgo restart
./ocgo status
./ocgo stop
./ocgo version
./ocgo version --remote
./ocgo update
```

默认行为：

- 监听地址：`127.0.0.1:3000`
- 默认后台运行
- 默认纳管 `opencode`
- 默认托管后端地址：`127.0.0.1:4096`
- 默认情况下 `start` 就等于“后台运行 + 纳管 opencode”

如果你要改成前台运行或使用外部后端：

```bash
./ocgo start --foreground
./ocgo start --external
./ocgo start --backend 127.0.0.1:4096
```

说明：

- `start` 会后台启动 `ocgo`，同时由 `ocgo` 拉起并托管 `opencode serve`
- `start --foreground` 适合 `systemd`、`supervisor`、Docker 这类外部进程管理器前台运行
- `start --external` 表示不纳管 `opencode`，并使用默认外部后端地址
- `start --backend ...` 表示不纳管 `opencode`，并转发到你指定的外部后端
- 如果本机还没有安装 `opencode`，托管启动时会自动下载对应平台的 CLI release 二进制并安装
- `restart` 会优先复用当前运行实例的监听参数、后端参数和托管参数，也支持通过 flags 覆盖
- `status` 会显示托管的 `opencode` 进程状态
- `stop` 会同时停止前端服务和它托管的 `opencode`
- 默认托管地址是 `127.0.0.1:4096`，可用 `--oc-host`、`--oc-port` 调整
- 可用 `--path` 指定 `opencode serve` 运行的项目目录；这对“部署程序目录”和“实际项目目录”不同的场景很重要
- 开启托管模式时，不再单独使用 `--backend` 指向别的地址；后端会固定到托管的 `opencode`

`opencode` 可执行文件获取顺序：

- `OPENCODE_BINARY`
- `OPENCODE_PATH`
- `OPENCHAMBER_OPENCODE_PATH`
- `OPENCHAMBER_OPENCODE_BIN`
- 常见安装位置：`~/.opencode/bin/opencode`、`~/.local/bin/opencode`、`/usr/local/bin/opencode`、`/opt/homebrew/bin/opencode`、`/usr/bin/opencode`
- 最后回退到当前 `PATH` 中的 `opencode`
- 如果以上都找不到，会自动下载 `anomalyco/opencode` 的对应 CLI release 资产安装

示例：

```bash
./ocgo start --host 0.0.0.0 --port 8080
./ocgo start --path /srv/my-project
./ocgo start --foreground --path /srv/my-project
./ocgo start --external
./ocgo start --backend 127.0.0.1:4096
./ocgo restart --port 8081
./ocgo restart --external
./ocgo restart --backend 127.0.0.1:5000
```

## 更新

```bash
./ocgo update
./ocgo update --oc-version v1.4.8
```

说明：

- `update` 也会检查本机 `opencode`；已安装时直接下载并替换到最新 release，未安装时自动安装
- 可用 `--oc-version` 指定把 `opencode` 固定更新到某个版本
- `update` 会逐项交互确认：是否更新 `ocgo`、是否更新 `opencode`，以及是否允许停服务重启；直接回车默认继续
- 如果原服务正在运行，更新成功后会按原监听参数和后端参数自动重新拉起
- 如果原服务正在纳管 `opencode`，更新后也会按原托管参数重新拉起
- 因为前端已嵌入二进制，所以不再有单独前端更新逻辑
- 下载 `ocgo` 和 `opencode` release 失败时，会自动尝试常见 GitHub 下载反代镜像
- 启动前会先检查监听地址是否可用，避免后台静默启动失败

## 密码兼容

兼容上游 OpenCodeUI 的服务密码模式。

- 上游前端在请求时会携带 `Authorization: Basic ...`
- WebSocket 也会带上认证信息
- 本工具的 `/api` 反代不会移除这些认证头

因此如果你的 `opencode serve` 设置了密码，这种部署方式仍然可以使用。

## 发布逻辑

本仓库 workflow 会：

1. 定时检查上游 `lehhair/OpenCodeUI` 最新 release
2. 拉取上游对应 tag
3. 构建前端
4. 将前端嵌入 Go 二进制
5. 用相同版本号在本仓库发布 Linux 二进制

所以这里的 release 本质上是“上游 release 的二进制封装版”，应该优先关注上游仓库的 changelog 和设计变更。

另外，workflow 也支持手动指定上游版本做一次 release，适合验证升级流程。
