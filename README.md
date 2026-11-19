# RdpMux

RdpMux 为 Windows 10/11 客户端版提供并发远程桌面会话。它通过内存补丁修改 `termsrv.dll` 的会话限制，并由常驻 Windows 服务持续校验和重新应用补丁。

## 原理与风险

RDP 协议栈本身支持多会话；客户端版受限主要来自软件许可（SL）策略。RdpMux 在 `termsrv.dll` 中修改四类检查点：

- `LocalOnly`：跳过“仅限本地”许可限制；
- `SingleUser`：解除同一用户的新连接必须接管现有会话的限制；
- `DefPolicy`：强制返回“允许”；
- `SLInit`：把 `CSLQuery` 的 8 个全局变量写成服务器版的值。

补丁只写入 TermService 进程内存，不修改磁盘上的 `termsrv.dll`。写入前会校验 PE 头、目标地址范围和原始字节；缺失或不完整的版本条目不会使用；批量写入失败时会尝试恢复本次修改并报告恢复错误。运行中修改代码仍有风险，恢复失败不能保证系统一定保持单会话。

## 组件

- `rdpmux.exe`：命令行工具，同时作为补丁服务运行。
- `rdpmuxgui.exe`：图形界面，始终以管理员权限运行；提供安装、卸载、更新 INI、重启服务和重启 TermService 按钮。

## 系统要求

- Windows 10 / Windows 11 客户端版（x64）。
- 目标系统的 `termsrv.dll` 版本必须包含在补丁库 `rdpwrap.ini` 中。
- 从源码构建需要 Go 1.27.3 或更高版本。

## 构建

```bat
build.bat
```

产物为 `bin\rdpmux.exe` 和 `bin\rdpmuxgui.exe`。

## 命令

| 命令 | 作用 | 管理员 |
|---|---|---|
| `rdpmux.exe install` | 启用远程桌面，部署文件并安装/启动补丁服务 | 是 |
| `rdpmux.exe uninstall` | 移除服务并恢复原版单会话行为（远程桌面保持启用） | 是 |
| `rdpmux.exe status` | 显示 termsrv 版本、补丁库日期、TermService、监听和补丁状态 | 否 |
| `rdpmux.exe update [url]` | 下载补丁库（默认 GitHub，也可传 HTTPS 地址），只写入不应用；已安装时用 `restart` 或重启电脑应用，未安装时用 `patch` | 是 |
| `rdpmux.exe restart` | 重启补丁服务以应用当前补丁库；若内存中的代码补丁与新库不兼容，会先重启 TermService（活动会话断开） | 是 |
| `rdpmux.exe patch` | 立即应用一次补丁（不通过服务） | 是 |
| `rdpmux.exe help` | 显示帮助 | 否 |

`update` 下载的 INI 含未签名的补丁字节，只应从可信来源更新。

未提权时，需要管理员的命令会通过 UAC 自动提权并等待结果。`status` 的补丁检查需要读取 TermService 内存，非管理员运行时只提示以管理员身份检查。

## 安装与卸载行为

- 安装是幂等的：已安装且程序文件一致时只确保服务在运行；不一致时先停服务，用当前目录中的 `rdpmux.exe`（以及存在时的 `rdpmuxgui.exe`）覆盖安装目录，保留 `rdpwrap.ini` 和 `rdpmux.conf`。重装不改 RDP 设置，也不重启 TermService。
- 全新安装会在 TermService 未运行时启动它；若其启动类型为 Disabled，会改为 Automatic。启用远程桌面后若监听器未出现，安装会在 TermService 原本已运行时重启一次 TermService；重启后仍无法监听则报错。
- 如果系统缺少 `%SystemRoot%\System32\rfxvmt.dll`，安装会写入该文件（写入失败仅告警）；卸载不会删除它。
- 卸载会移除补丁服务并重启 TermService（活动 RDP 会话断开），恢复原版单会话行为。`fDenyTSConnections`、`EnableConcurrentSessions`、`AllowMultipleTSSessions`、`Terminal Server\AddIns`、`CertPropSvc`/`SessionEnv` 启动类型，以及 TermService 的启动类型（若安装时由 Disabled 改为 Automatic）都不会回滚。
- 无法立即删除的程序或日志文件会安排在下次重启后删除；上次卸载中断的残留可通过再次卸载继续清理。
- 安装、卸载、重启服务和重启 TermService 都在临时 SYSTEM 服务中执行，客户端只负责提交并显示日志，因此 RDP 会话断开不会中断操作。

## 运行要求

补丁只修改内存，以下情况会丢失：重启电脑、TermService 重启、Windows 更新替换 `termsrv.dll`。安装并运行补丁服务时，服务会在 TermService 恢复且版本仍受支持时自动重新应用；未安装服务时需手动执行 `rdpmux.exe patch`。

- 补丁服务需要常驻，安装后设为开机自启；每 5 秒校验一次，发现未打补丁会重新应用。
- 常驻补丁服务不会主动启动 TermService，只在其运行时附加。
- GUI/CLI 只是管理工具，用完可关闭，不影响多会话。
- 停止补丁服务后，当前补丁仍有效，但下次重启或 TermService 重启后不再自动恢复。
- 不安装服务时，可在每次开机后执行 `rdpmux.exe patch`，效果持续到下次重启或 TermService 重启。

## 语言与日志

支持 `en-US`、`zh-CN`。语言选择顺序：CLI 的 `-lang` 参数、注册表 `HKCU\Software\RDP Wrapper\Language`、Windows 显示语言、`en-US`。内嵌英文为基底，外部 `lang\en-US.ini`、内嵌目标语言、外部 `lang\<目标>.ini` 依次覆盖；缺失键回退英文。

```bat
rdpmux.exe status -lang zh-CN
```

日志位于 exe 同目录的 `rdpmux.log`，达到 5 MB 后轮转并保留 `rdpmux.log.1`。级别优先级：`-v`/`-vv`/`-log-level` > 环境变量 `RDPMUX_LOG_LEVEL` > `rdpmux.conf` 的 `log-level=`。

## 配置与故障排查

- 安装目录（`%ProgramFiles%\RdpMux`）下的 `rdpmux.conf` 可设置日志级别；RDP 端口、NLA、会话影子等通过图形界面修改。
- RdpMux 不管理 Windows 防火墙规则。旧版本可能留下 `RdpMux TCP/UDP` 规则，如需清理可执行 `netsh advfirewall firewall delete rule name="RdpMux TCP"` 和 `netsh advfirewall firewall delete rule name="RdpMux UDP"`。
- 监听状态通过查询本机 TCP 监听表确认，不主动建立连接。全新安装和 TermService 重启后会校验配置端口确实在监听；重复安装不会重复校验。
