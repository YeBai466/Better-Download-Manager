# B Download Manager

一个仅面向 **Windows** 的仿 IDM 多线程下载器，使用 **Go + React (Wails v3)** 构建。
单文件、小体积（使用系统 WebView2，不打包 Chromium），支持：

- 🚀 **多线程分段下载**（HTTP Range，默认 8 连接）
- ⏸ **断点续传**（崩溃安全的 `.bdmeta` sidecar + SQLite 持久化）
- 🌐 **浏览器接管**（Chrome / Edge / Firefox 扩展，拦截下载并转交本程序）
- 🛡 **代理支持**（系统代理 / 自定义 HTTP(S) / SOCKS5）
- 🗂 **IDM 风格 UI**（工具栏、分类侧栏、任务表、添加对话框、系统托盘）

## 环境要求

- Go 1.25+（Wails v3 要求）
- Node.js 20+
- Windows 10/11 + WebView2 运行时（系统通常已内置）
- Wails v3 CLI：`go install github.com/wailsapp/wails/v3/cmd/wails3@latest`

## 开发与构建

```bash
wails3 dev      # 开发模式（前后端热重载）
wails3 build    # 生产构建 -> bin/b-download-manager.exe
wails3 package  # 生成 NSIS 安装包（需先安装 NSIS：winget install NSIS.NSIS）
```

仅运行 Go 单元测试：

```bash
go test ./internal/...
```

## 项目结构

```
main.go                 应用入口：窗口、系统托盘、服务装配
internal/
  downloader/           核心下载引擎（探测/分段/续传/调度），与 UI 解耦、可独立测试
  httpclient/ proxy/    代理感知的 HTTP 客户端（系统/自定义/SOCKS5）
  store/                SQLite 持久化（纯 Go modernc 驱动）
  category/             文件分类与落盘目录路由
  takeover/             浏览器接管本地 HTTP 服务
  service/              Wails 绑定服务（暴露给前端的方法 + 事件）
  config/               设置模型与默认值
frontend/src/           React + TS UI（components / api / format / styles）
extensions/
  chromium/             Chrome / Edge 扩展（Manifest V3）
  firefox/              Firefox 扩展（WebExtension）
```

## 安装浏览器扩展

应用运行时会在 `127.0.0.1:9614` 启动接管服务（可在「选项 → 浏览器接管」修改端口/开关）。

**Chrome / Edge（推荐：静默安装）**：启动应用时若检测到未安装扩展，会弹窗询问——可选择「立即安装 / 下次再说 / 从此忽略」。
点「立即安装」后通过浏览器企业策略静默装入（需管理员授权 UAC，完成后重启浏览器生效），无需打开商店或手动加载。
也可随时在「选项 → 浏览器接管」里安装 / 卸载扩展。**不会在未经同意的情况下自动安装。**

> 实现方式：应用把扩展打包成签名 CRX3，由本地服务（`/ext/updates.xml`、`/ext/bdm.crx`）提供，并写入
> `HKLM\...\Policies\{Google\Chrome,Microsoft\Edge}\ExtensionInstallForcelist` 完成强制安装。

**Firefox（手动加载）**：`about:debugging#/runtime/this-firefox` →「临时加载附加组件」→ 选择 `extensions/firefox/manifest.json`。

扩展会取消浏览器自身下载并转交本程序（按设置弹出添加对话框或直接开始）；若程序未运行，扩展自动回退为浏览器原生下载。

## 测试覆盖

- `internal/downloader`：分段下载、无 Range 单流回退、断点续传、暂停/恢复（含 `-race`）
- `internal/service`：添加→下载→落盘→持久化全链路
- `internal/takeover`：接管 HTTP 接口（/ping、/download）与端口热切换
- `internal/browserext`：CRX3 打包/签名正确性、扩展 ID 稳定性、update.xml
