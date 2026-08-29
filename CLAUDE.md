# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目

Better Download Manager（`bdm`）—— 面向 Windows 的 IDM 风格多线程下载器，Go + React，基于 **Wails v3 alpha**。复用系统 WebView2，发行版是单个 exe。Go module：`github.com/yebai/better-download-manager`。

## 工作流规范（必须遵守）

- **大改动前先 commit。** 动手做涉及多文件、重构、引擎/服务层改造之类的大改动之前，先把当前工作区已有的改动提交掉，保证有一个干净的回退点。工作区是脏的就先 commit，不要在未提交的改动之上叠加大改动。
- **做完验证没问题就 push。** 改动完成并且验证通过（能编译、相关 `go test` 通过、必要时实际跑过 app）之后，提交并 push 到远端。不要把验证过的成果只留在本地。
- **release 前先问我。** 不要自作主张发版。打包安装包、创建 tag、发 GitHub Release 之前，先问用户「要不要 release」，得到明确答复再执行。
- 验证不通过就如实说明失败原因，不要当作完成。

## 命令

```bash
wails3 dev                  # 开发模式，前后端热重载（Vite 127.0.0.1:9245，配置 build/config.yml）
wails3 build                # 生产构建 -> bin/better-download-manager.exe
wails3 package              # 生成 NSIS 安装包（需先装 NSIS：winget install NSIS.NSIS）
wails3 task run             # 运行已构建的 exe

go test ./internal/...                                    # 全部 Go 测试
go test ./internal/downloader -run TestResumeFromMeta -v  # 跑单个测试
go test ./internal/downloader -race                       # 引擎并发测试
go test ./internal/downloader -bench=. -run '^$'          # BenchmarkDownloadEngine

cd frontend && npm run build   # tsc + vite build（wails3 build 会自动做）
```

环境要求：Go 1.25+、Node 20+、`wails3`（`go install github.com/wailsapp/wails/v3/cmd/wails3@latest`）。项目没有配置 linter，只有 `gofmt` / `go vet`。

## Wails 绑定（改前端之前必读）

`frontend/bindings/` 是**生成产物且被 gitignore**。`wails3 build` / `dev` 会执行 `wails3 generate bindings -clean=true -ts -i`，先清空再重新生成。绝不要手改这些文件；import 报找不到模块时，跑一次 build 即可。

由于用了 `-ts -i`，生成的 model 是 **TypeScript interface 而不是 class** —— 用 `import type` 引入，直接写对象字面量。不要写 `new Model(...)` 或 `Model.createFrom(...)`。

新增一个前端可调用的后端方法：
1. 在 `internal/service/download_service.go` 的 `*DownloadService` 上加导出方法（参数/返回值必须是可绑定的类型）。
2. 重新 build 以重新生成绑定。
3. 在 `frontend/src/api.ts` 里加一层薄封装 —— UI 只调 `api.*`，永远不直接调生成的 service。

新增后端 → 前端事件：在 `internal/service` 定义事件名常量、发射事件，**并且必须在 `main.go` 的 `init()` 里注册**（`application.RegisterEvent[T]`），否则生成器不会产出对应的类型化 API。前端通过 `api.ts` 里的 `onEvent()` 订阅。

## 架构

严格分层 —— 下载引擎完全不知道 UI、存储和 Wails 的存在：

- `internal/downloader` —— 下载引擎。纯 Go，可独立测试，只通过 `downloader.Config` 里的回调（`OnUpdate`、`OnPersist`、`OnRemoved`）和 `ClientFactory` 与外界通信。
- `internal/service` —— 唯一的 Wails 绑定服务（`DownloadService`）。持有引擎、store 和接管服务；把上述回调接到事件和 SQLite 上；负责窗口、设置、更新、扩展安装。
- `internal/store` —— SQLite 持久化（`modernc.org/sqlite`，无 CGO），WAL 模式，`SetMaxOpenConns(1)` 串行化写入。
- `internal/takeover` —— 仅监听回环地址的 HTTP 服务（`/ping`、`/download`），浏览器扩展往这里 POST。
- `internal/policy`（Windows 专属 build tag）—— 通过 HKLM 策略注册表键强制安装 Chrome/Edge 扩展；写入需要重新拉起提权 PowerShell（一次 UAC），读取不需要。
- `internal/{httpclient,proxy,config,category,updates}` —— 支撑包。
- `main.go` —— 窗口、托盘、单实例（`UniqueID: com.yebai.betterdownloadmanager`）、事件注册、`%AppData%\BetterDownloadManager\bdm.db`。

### 下载引擎模型

探测（ranged `GET bytes=0-0`，比 HEAD 可靠得多）→ 分块计划 → 传输。有两个容易混淆的并行概念：

- **`Chunk`** —— 持久化的下载计划。被持久化、重试、续传的是 chunk；动态调度器从队列里把 chunk 分发给 worker。
- **`Segment`** —— 只是 **UI 通道**（进度对话框里每条线程的进度行），不是实际的工作单位。

`fastStartV2` 在流式读取第一个响应体的同时并行铺开剩余 chunk，因此下载立刻跑满速度，而不是先浪费一个往返去探测。`engine.go` 里的 `fastStartLegacy` / `transferLegacy` 是旧的单计划路径，保留但已不可达 —— `fastStart` / `transfer` 都转发到 V2 版本。

续传状态**故意存两份**：紧挨 `.part` 文件的 `.bdmeta` JSON sidecar（数据库丢失或损坏也能恢复）+ SQLite 记录。续传时引擎会重新校验 ETag / Last-Modified / 大小 / 最终 URL，任一不匹配就从头下载。`.part` 文件**故意不预分配** —— 在 Windows 上遇到杀软和过滤驱动时，大文件 `Truncate` 慢得离谱。

连接数随文件大小自适应（`internal/downloader/policy.go`）：<8 MiB 用 1 连接，随后在跨过 64 MiB、512 MiB 时依次为 2 / 4 / 用户请求的完整值（默认 8）。分块大小同样按 1 / 8 / 16 MiB 递增。

`internal/httpclient` **故意禁用 HTTP/2**（`TLSNextProto: map[...]{}`）—— 把所有 range 请求复用到一条 TCP 连接上会彻底废掉多连接模型。不要"修"这个。HTTP client 是代理感知的，并在 service 里按 `proxy.Settings` 缓存，因为代理是每任务级别的设置。

### 前端 / 多窗口

所有窗口共用同一份 React 包，路由靠 `main.tsx` 读取的 query 参数区分 —— `/?view=add&w=<name>` 渲染 `AddPage`，其余渲染 `App`。

每次"添加下载"都会开一个**全新的、名字唯一的窗口**（`add-1`、`add-2`……），这样可以像 IDM 一样同时配置多个下载。预填数据按窗口名存在 `pendingAdds` 里，由该窗口自己通过 `ConsumePendingAdd(window)` 取走 —— 不要假设只有一个共享的添加窗口。

i18n（`frontend/src/i18n.ts`）是模块级当前语言 + 订阅 hook；在每个**窗口根组件**里调 `useLang()`，语言切换时整棵树才会重渲染。原生外壳（托盘菜单、窗口标题、Go 侧对话框）只在启动时通过 `main.go` 的 `tr()` / service 的 `s.tr()` 本地化一次，改语言要下次启动才生效。

### 浏览器接管

程序监听 `127.0.0.1:9614`（端口和开关可配置）。扩展取消浏览器自带下载并把请求 POST 过来；程序没运行时扩展静默回退为原生下载。`TakeoverAction` 决定弹对话框还是直接开始。扩展源码在 `extensions/chromium`（MV3，同时被 `go:embed` 进 exe 供手动"加载已解压的扩展程序"）和 `extensions/firefox`。已发布的商店 ID `hgkakilajhnbpmhmpcnblioiiaomdkjp` 硬编码为 `service.StoreExtensionID`，必须与 `policy` 里的强制安装项保持一致。

## 发版 / 版本号

版本号在五处重复，必须全部一致，否则更新器和安装包会对不上：

1. `build/config.yml` → `info.version`
2. `build/windows/info.json` → `file_version` + `ProductVersion`
3. `build/windows/nsis/project.nsi` → `INFO_PRODUCTVERSION`
4. `internal/service/download_service.go` → `const Version`（上报给扩展，也用于更新检查）
5. `frontend/src/App.tsx` → `APP_VERSION`（关于对话框）

`build/windows/nsis/project.nsi` **必须保持纯 ASCII** —— 出现非 ASCII 字节会让 `makensis` 报 "Bad text encoding" 失败。它产出的安装包文件名正是 `internal/updates` 在 GitHub Release（`YeBai466/Better-Download-Manager`）里查找的资产名，改名会直接破坏应用内更新。

应用内更新会把安装包下载到 `<安装目录>/tmp` 并运行它覆盖安装；`CleanupUpdateTmp()` 在启动时清理残留。

## 约定

- 注释解释**为什么**，尤其是那些"看起来写错了其实是故意的"地方（不预分配、强制 HTTP/1.1、`MaxOpenConns(1)`、每次新开添加窗口）。保持同样的密度 —— 简短、有信息量，不要复述代码。
- `%AppData%\BetterDownloadManager` 里的用户数据，安装和卸载都不得动。
- `internal/policy` 按 build tag 拆分（`policy_windows.go` / `policy_other.go`），改动时保证非 Windows 桩代码仍能编译。
- 测试用 `httptest` 模拟支持 range / 不支持 range / 不稳定的服务器 —— 扩展这些 helper，不要真的走网络。
