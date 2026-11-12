# RdpMux

RdpMux 为 Windows 客户端版本提供并发远程桌面会话支持。它通过内存补丁修改 `termsrv.dll` 的会话限制，并由 Windows 服务持续维护补丁状态，因此系统更新或服务重启后无需手动重新打补丁。

## 多会话原理

客户端版与服务器版使用**同一份 `termsrv.dll`**，RDP 协议栈本身支持多会话；差别在于 termsrv.dll 运行时读取的软件许可（SL）策略值——消费版被设为“只允许 1 个交互会话”，服务器版允许并发。

补丁把守这个限制的几个检查点改成服务器版的行为：

- `CEnforcementCore::GetInstanceOfTSLicense`：跳过“仅限本地”许可限制；
- `CSessionArbitrationHelper::IsSingleSessionPerUserEnabled`：解除新连接必须接管/踢掉现有会话的仲裁；
- `CDefPolicy::Query`：强制返回“允许”；
- `CSLQuery::Initialize` 的 8 个全局变量（`bServerSku`、`bRemoteConnAllowed`、`lMaxUserSessions` 等）：写成服务器版的值，这是真正的开关。

RdpMux 采用**外部补丁**：不向 svchost 注入 DLL，而是由服务从外部写入目标进程内存。写入前校验加载镜像、地址范围和整组目标字节；缺失或不完整的版本条目不使用，写入失败时尝试恢复本次修改。运行中修改代码仍有风险，恢复失败也会明确报错，不能保证所有故障都只会保持单会话。

## 组件

- `rdpmux.exe`：命令行工具，同时作为补丁服务运行。
- `rdpmuxgui.exe`：图形界面，用于查看状态、修改 RDP 设置和执行安装/卸载等操作。

## 系统要求

- Windows 10 / Windows 11 客户端版（x64）。
- 目标系统的 `termsrv.dll` 版本必须包含在补丁库 `rdpwrap.ini` 中。
- 从源码构建需要 Go 1.27 或更高版本。

## 构建

```bat
build.bat
```

产物位于 `bin\`：

- `rdpmux.exe`
- `rdpmuxgui.exe`
- `RdpMux-win64.zip`

## 使用

安装（会触发 UAC 提权）：

```bat
rdpmux.exe install
```

查看状态（无需管理员权限）：

```bat
rdpmux.exe status
```

`status` 可以显示 `termsrv.dll` 版本、补丁库日期、TermService 状态和监听状态。补丁是否真正生效需要读取 TermService 进程内存，因此该检查需要管理员权限；非管理员运行时会提示以管理员身份检查。

更新补丁库：

```bat
rdpmux.exe update
```

立即应用一次补丁（不通过服务）：

```bat
rdpmux.exe patch
```

卸载：

```bat
rdpmux.exe uninstall
rdpmux.exe uninstall -k   # 保留 RDP 设置
```

卸载会重启 TermService，当前活动的 RDP 会话将断开。无法立即删除的自身程序或日志文件会安排在下次重启后删除。

图形界面的操作与命令行一致，安装、卸载、更新和重启 TermService 按钮位于主窗口中。语言下拉框会立即切换界面语言，尚未保存的设置和日志内容不会丢失。

## 运行要求

补丁只修改内存，不改动磁盘上的 `termsrv.dll`。因此在以下情况补丁会丢失，需要重新应用：**重启电脑**、**TermService 服务重启**、**Windows 更新替换 termsrv.dll**。

- **RdpMux 服务**：需要常驻。安装后设为开机自启的后台服务，自动校验并重新应用补丁，无需手动保持任何窗口打开。服务很轻量（几 MB 内存，每 5 秒校验一次）。
- **GUI / CLI**：只是管理工具，用完即可关闭，不影响多会话。
- **停止 RdpMux 服务**：当前已生效的补丁仍保持多会话，但下次重启或 TermService 重启后不再自动恢复。
- **不想常驻服务**：可不安装服务，改为每次开机后手动执行 `rdpmux.exe patch` 应用一次，效果持续到下次重启。

## 语言

支持 `en-US` 和 `zh-CN`。默认按以下顺序选择：

1. `-lang` 参数；
2. 注册表 `HKCU\Software\RDP Wrapper\Language` 中保存的选择；
3. 当前 Windows 显示语言；
4. `en-US`。

```bat
rdpmux.exe status -lang zh-CN
rdpmux.exe help -lang en-US
```

内嵌的英文语言文件作为基底，目标语言和外部文件逐层覆盖。可将自定义的 `en-US.ini` 或 `zh-CN.ini` 放在 exe 同目录的 `lang\` 下进行覆盖；缺失的键会回退到英文，不会显示为空。

## 日志

日志文件为 exe 同目录的 `rdpmux.log`，达到 5 MB 后轮转并保留 `rdpmux.log.1`。

日志级别可通过以下方式设置（优先级从高到低）：

- 命令行：`-v`（debug）、`-vv`（trace）、`-log-level <error|warn|info|debug|trace>`；
- 环境变量：`RDPMUX_LOG_LEVEL`；
- 配置文件：exe 同目录的 `rdpmux.conf` 中的 `log-level=`。

## 权限说明

- `help`、`status`：以当前用户权限运行，不触发 UAC。
- `install`、`uninstall`、`update`、`patch`：需要管理员权限，未提权时会通过 UAC 自动提权并等待结果。
- `rdpmuxgui.exe`：始终以管理员权限运行，因为 GUI 的安装、卸载和状态检查都需要管理员权限。

## 配置

安装目录下的 `rdpmux.conf` 可设置日志级别；RDP 端口、NLA、会话影子等设置可通过图形界面修改。
