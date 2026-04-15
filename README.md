# opencodeui

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

下载对应平台的二进制后：

```bash
./opencodeui start
./opencodeui status
./opencodeui stop
./opencodeui version
./opencodeui update
```

默认行为：

- 监听地址：`127.0.0.1:3000`
- 后端地址：`127.0.0.1:4096`

示例：

```bash
./opencodeui start --ip 0.0.0.0 --port 8080
./opencodeui start --backend 127.0.0.1:4096
```

## 更新

```bash
./opencodeui update
```

说明：

- `update` 只更新工具二进制
- 如果服务正在运行，会拒绝更新，需先 `stop`
- 因为前端已嵌入二进制，所以不再有单独前端更新逻辑

## 发布逻辑

本仓库 workflow 会：

1. 定时检查上游 `lehhair/OpenCodeUI` 最新 release
2. 拉取上游对应 tag
3. 构建前端
4. 将前端嵌入 Go 二进制
5. 用相同版本号在本仓库发布 Linux 二进制

所以这里的 release 本质上是“上游 release 的二进制封装版”，应该优先关注上游仓库的 changelog 和设计变更。
