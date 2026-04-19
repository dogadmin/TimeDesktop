# TimeDesktop

Windows 桌面多时区悬浮窗，单文件 Go（无 CGO），体积约 6.9 MB。

![preview](https://img.shields.io/badge/platform-Windows-blue) ![go](https://img.shields.io/badge/go-1.22%2B-00ADD8) ![cgo](https://img.shields.io/badge/CGO-disabled-brightgreen)

## 特点

- **单文件**：主程序只有一个 `main.go`，不依赖 Fyne / Qt / Electron / webview。
- **纯 Go、零 CGO**：Win32 + GDI 通过 `golang.org/x/sys/windows` 的 LazyProc 调用；Windows 本机/交叉编译都不需要 gcc / MinGW。
- **不落盘**：状态全部写到 Windows 注册表 `HKCU\Software\DesktopTime`，不产生任何配置文件、缓存文件、资源文件。
- **真正的本地时区名**：启动时读 `HKLM\SYSTEM\...\TimeZoneInformation\TimeZoneKeyName`，通过内嵌的 CLDR 映射表（140+ 条）换成 IANA 名（如 `Asia/Shanghai`），再在城市表里取「上海 / Shanghai」展示；**不是**那种只给你一个 `UTC+8` 就完事的东西。
- **覆盖全球**：260+ 个 IANA 时区，按大洲分子菜单（非洲 / 美洲 / 亚洲 / 欧洲 / 大洋洲 / 南极 / 印度洋 / 大西洋 / 太平洋 / UTC 偏移）。
- **中英双语**：每个城市、每个大洲、每条菜单项都带 `cn` + `en`，右键菜单里一键切换。
- **联网校时**：主用 `worldtimeapi.org`（HTTPS），备用 Google `Date` 响应头；3 秒超时，全部失败就静默回落系统时钟，UI 永不卡。
- **边缘停靠**（v1.1）：一键吸附到屏幕任意一边，收缩成纯色小条；鼠标悬停自动展开为完整时钟，离开折回。可固定展开，可拖离边缘自动脱离。
- **系统托盘 + 全局热键**（v1.1）：托盘图标始终驻留，`Ctrl+Alt+T` 随时切换显示/隐藏，`Explorer` 重启后自动恢复托盘。
- **自定义外观**（v1.1）：6 种预设色 + 系统调色板任选边缘条颜色；折叠态透明度与完整时钟独立调节；自带米色表盘蓝圆环的应用图标，嵌入到 exe 资源。
- **无边框置顶**：`WS_EX_TOPMOST | WS_EX_LAYERED | WS_EX_TOOLWINDOW`，不出现在 Alt+Tab。
- **自适应宽度**：GDI 实测每行 label 和时间字符串像素宽度，加/删时区、切字号、切语言后窗口自动贴合。

## 下载

去 [Releases](https://github.com/dogadmin/TimeDesktop/releases/latest) 页下载 `TimeDesktop.exe`，双击即跑，没有安装步骤，没有运行时依赖。

## 使用

### 基本操作

- **左键拖动**：移动窗口（停靠模式下拖离边缘 > 40px 会自动退出停靠）。
- **右键**：弹出菜单。
- **托盘图标**：左键切换隐藏，右键弹出相同菜单。
- **`Ctrl+Alt+T`**：任何时候切换显示 / 隐藏（全局热键）。

### 菜单项

- **添加时区** → 选大洲 → 选城市（最多 10 个，重复的会置灰）
- **删除时区**
- **显示秒**
- **字号**：小 (13) / 中 (16) / 大 (20) / 特大 (24)
- **透明度**：100% / 90% / 80% / 70% / 60%（应用于完整时钟态）
- **主题**：深色 / 浅色
- **语言 / Language**：中文 / English（城市名、菜单项全量切换）
- **停靠**
  - **边缘停靠**：开/关吸附模式
  - **吸附到 上 / 下 / 左 / 右 边**
  - **固定展开**：锁定为常驻时钟，不自动折回
  - **隐藏 (Ctrl+Alt+T)**
  - **边缘色**：蓝 / 紫 / 青 / 绿 / 橙 / 粉 / 自定义…（Windows 调色板）
  - **小条透明度**：100% / 80% / 60% / 40% / 25%
- **恢复默认**
- **退出**

窗口位置、字号、透明度、主题、语言、时区列表、停靠配置、边缘色、小条透明度全部自动持久化到注册表，下次开机接着用（`Hidden` 状态每次启动复位为可见，避免找不到窗口）。

## 从源码编译

```bash
git clone https://github.com/dogadmin/TimeDesktop.git
cd TimeDesktop
go mod tidy

# 本机 Windows amd64 编译
CGO_ENABLED=0 go build -ldflags="-s -w -H windowsgui" -o TimeDesktop.exe

# 交叉编译 arm64（不需要 C 编译器）
CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build -ldflags="-s -w -H windowsgui" -o TimeDesktop_arm64.exe
```

`-H windowsgui` 把子系统标成 GUI，启动时不会弹黑色终端。

仓库里已经提交了生成好的 `icon.ico` 和 `rsrc.syso`（Go 会自动把 .syso 嵌进 exe）。如果想改图标，重新生成即可：

```bash
go run ./tools/genicon                    # 产出 icon.ico
go install github.com/akavel/rsrc@latest  # 一次性安装
rsrc -ico icon.ico -o rsrc.syso           # 产出资源文件
go build ...                              # 重编
```

`tools/genicon` 是一个独立的 Go 程序，用 `image` + `image/png` 从零画出米色表盘 + 蓝圆环 + 10:10 指针，打包成多尺寸 (16/24/32/48/64/128/256) 的 PNG-in-ICO。

## 技术栈

- Go 1.22+
- `golang.org/x/sys/windows` + `golang.org/x/sys/windows/registry`
- `_ "time/tzdata"`：把 IANA 时区库静态嵌到二进制里，目标机器没有 zoneinfo 也能跑
- Win32 消息循环 + `runtime.LockOSThread()`：消息循环必须钉在单一 OS 线程上，否则 Go 调度器跨线程调度 `syscall.NewCallback` 回调会挂死
- `TrackPopupMenu` + `TPM_RETURNCMD`：跳过 `WM_COMMAND` 分发，直接拿命令 ID
- `ReleaseCapture` + `WM_NCLBUTTONDOWN(HTCAPTION)`：让 DWM 原生处理无边框窗口的拖动
- `Shell_NotifyIconW` + `TaskbarCreated` 广播：托盘图标 + Explorer 崩溃恢复
- `RegisterHotKey` + `WM_HOTKEY`：全局快捷键
- `TrackMouseEvent` + `WM_MOUSELEAVE`：悬停边界检测，配合 120 ms 宽限定时器避免抖动
- `MonitorFromWindow` + `GetMonitorInfoW`：多显示器 + 任务栏让位（用 `rcWork` 而非 `rcMonitor`）
- `ChooseColorW`（comdlg32）：系统自带调色板对话框
- `rsrc.syso`：`github.com/akavel/rsrc` 生成的 COFF 资源对象文件，被 Go 自动嵌入 exe，使资源管理器 / 任务栏 / 托盘都能显示同一个图标

## 已知限制

- **仅 Windows**。macOS / Linux 不支持。
- Windows Vista 之前的老系统没有 `TimeZoneKeyName` 注册表值，本机时区会退回显示 `UTC±HH:MM`。
- 透明度作用于整个窗口（`LWA_ALPHA`），文字也会跟着半透。想要文字实色 + 背景透明需要 `UpdateLayeredWindow`，复杂度高，暂未实现。
- 字体固定为 `Microsoft YaHei`；系统没装就走 GDI 默认回退。
- `Asia/Choibalsan` 在 tzdata 2024a 合并进了 `Asia/Ulaanbaatar`，仍可用（走向后兼容 alias）。
- 全局热键 `Ctrl+Alt+T` 固定不可配置；与其它程序冲突时需对方让位。

## 设计取舍

- 为什么不用 Fyne / Gio / webview？—— 都要 CGO，Windows 本机要装 MinGW，且会产生缓存目录，违反「单文件 + 无配置文件」的原则。
- 为什么不 pure-Go 实现 macOS？—— 要靠 `purego` 动态调 AppKit，2000+ 行脆弱手写代码，不值得。
- 为什么注册表而不是 JSON/YAML？—— 用户明确要求「不产生任何配置文件」。注册表是 Windows 上最自然的键值存储。
- 为什么城市名硬编码双语 + 手写 CLDR 映射？—— 避免引入 `go-text` / `cldr` 这类大依赖；表一次性录入，改起来也简单。
- 为什么停靠小条是纯色条而不显示缩略时间？—— 小条竖放时文字无法斜排；统一成纯色条避免上/下 vs 左/右 两套渲染分支，配色的识别度靠自定义颜色解决。
- 为什么 `Hidden` 不持久化？—— 避免用户从托盘隐藏后重启找不到程序，最安全的默认是可见。

## License

MIT
