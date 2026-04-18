# TimeDesktop

Windows 桌面多时区悬浮窗，单文件 Go（无 CGO），体积约 6.8 MB。

![preview](https://img.shields.io/badge/platform-Windows-blue) ![go](https://img.shields.io/badge/go-1.22%2B-00ADD8) ![cgo](https://img.shields.io/badge/CGO-disabled-brightgreen)

## 特点

- **单文件**：整个项目只有一个 `main.go`，不依赖 Fyne / Qt / Electron / webview。
- **纯 Go、零 CGO**：Win32 + GDI 通过 `golang.org/x/sys/windows` 的 LazyProc 调用；Windows 本机/交叉编译都不需要 gcc / MinGW。
- **不落盘**：状态全部写到 Windows 注册表 `HKCU\Software\DesktopTime`，不产生任何配置文件、缓存文件、资源文件。
- **真正的本地时区名**：启动时读 `HKLM\SYSTEM\...\TimeZoneInformation\TimeZoneKeyName`，通过内嵌的 CLDR 映射表（140+ 条）换成 IANA 名（如 `Asia/Shanghai`），再在城市表里取「上海 / Shanghai」展示；**不是**那种只给你一个 `UTC+8` 就完事的东西。
- **覆盖全球**：260+ 个 IANA 时区，按大洲分子菜单（非洲 / 美洲 / 亚洲 / 欧洲 / 大洋洲 / 南极 / 印度洋 / 大西洋 / 太平洋 / UTC 偏移）。
- **中英双语**：每个城市、每个大洲、每条菜单项都带 `cn` + `en`，右键菜单里一键切换。
- **联网校时**：主用 `worldtimeapi.org`（HTTPS），备用 Google `Date` 响应头；3 秒超时，全部失败就静默回落系统时钟，UI 永不卡。
- **无边框置顶**：`WS_EX_TOPMOST | WS_EX_LAYERED | WS_EX_TOOLWINDOW`，不出现在 Alt+Tab。
- **自适应宽度**：GDI 实测每行 label 和时间字符串像素宽度，加/删时区、切字号、切语言后窗口自动贴合。
- **左键拖，右键菜单**。

## 下载

去 [Releases](https://github.com/dogadmin/TimeDesktop/releases) 页拿对应架构的 `.exe`：

- `worldclock_windows_amd64.exe` —— Intel/AMD 64 位（绝大多数桌面 PC）
- `worldclock_windows_arm64.exe` —— ARM64（Surface Pro X、骁龙笔记本等）

下载下来双击就能跑，没有安装步骤，没有运行时依赖。

## 使用

- **左键拖动**：移动窗口。
- **右键**：弹出菜单。
  - **添加时区** → 选大洲 → 选城市（最多 10 个，重复的会置灰）
  - **删除时区** → 选要删的那条
  - **显示秒**
  - **字号**：小 (13) / 中 (16) / 大 (20) / 特大 (24)
  - **透明度**：100% / 90% / 80% / 70% / 60%
  - **主题**：深色 / 浅色
  - **语言 / Language**：中文 / English（城市名、菜单项全量切换）
  - **恢复默认**
  - **退出**

窗口位置、字号、透明度、主题、语言、时区列表全部自动持久化到注册表，下次开机接着用。

## 从源码编译

无依赖（除了 Go 本身）。

```bash
git clone https://github.com/dogadmin/TimeDesktop.git
cd TimeDesktop
go mod tidy

# Windows amd64（本机编译）
CGO_ENABLED=0 go build -ldflags="-s -w -H windowsgui" -o worldclock_windows_amd64.exe main.go

# Windows arm64（本机或交叉编译，都不需要 C 编译器）
CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build -ldflags="-s -w -H windowsgui" -o worldclock_windows_arm64.exe main.go
```

`-H windowsgui` 把子系统标成 GUI，启动时不会弹黑色终端。

## 技术栈

- Go 1.22+
- `golang.org/x/sys/windows` + `golang.org/x/sys/windows/registry`
- `_ "time/tzdata"`：把 IANA 时区库静态嵌到二进制里，目标机器没有 zoneinfo 也能跑
- Win32 消息循环 + `runtime.LockOSThread()`：消息循环必须钉在单一 OS 线程上，否则 Go 调度器跨线程调度 `syscall.NewCallback` 回调会挂死
- `TrackPopupMenu` + `TPM_RETURNCMD`：跳过 `WM_COMMAND` 分发，直接拿命令 ID
- `ReleaseCapture` + `WM_NCLBUTTONDOWN(HTCAPTION)`：让 DWM 原生处理无边框窗口的拖动

## 已知限制

- **仅 Windows**。macOS / Linux 不支持。
- Windows Vista 之前的老系统没有 `TimeZoneKeyName` 注册表值，本机时区会退回显示 `UTC±HH:MM`。
- 透明度作用于整个窗口（`LWA_ALPHA`），文字也会跟着半透。想要文字实色 + 背景透明需要 `UpdateLayeredWindow`，复杂度高，暂未实现。
- 字体固定为 `Microsoft YaHei`；系统没装就走 GDI 默认回退。
- `Asia/Choibalsan` 在 tzdata 2024a 合并进了 `Asia/Ulaanbaatar`，仍可用（走向后兼容 alias）。

## 设计取舍

- 为什么不用 Fyne / Gio / webview？—— 都要 CGO，Windows 本机要装 MinGW，且会产生缓存目录，违反「单文件 + 无配置文件」的原则。
- 为什么不 pure-Go 实现 macOS？—— 要靠 `purego` 动态调 AppKit，2000+ 行脆弱手写代码，不值得。
- 为什么注册表而不是 JSON/YAML？—— 用户明确要求「不产生任何配置文件」。注册表是 Windows 上最自然的键值存储。
- 为什么城市名硬编码双语 +手写 CLDR 映射？—— 避免引入 `go-text` / `cldr` 这类大依赖；表一次性录入，改起来也简单。

## License

MIT
