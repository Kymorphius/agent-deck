# Windows 原生终端原型

<!-- matrix/hardened: document the isolated experiment without claiming that the complete app is ported. -->

`agent-deck-native` 是独立的 Windows 原生终端实验，版本为
`0.2.0+matrix.prototype`。它不依赖 WSL 或 tmux，使用 ConPTY 启动
交互式子进程，通过本机命名管道提供输入、输出、截图、缩放和停止操作。
第二阶段增加了独立的中文多会话列表、SQLite 注册库和管理进程；
尚未接入现有 Agent Deck 的分组、生产数据库、Web 页面或工具状态识别。
现有 macOS/Linux 程序继续沿用原来的入口。

## 构建

最低系统要求是支持 ConPTY 的 Windows 10 1809 或更新版本。推荐在现代
Windows Terminal 内操作 `attach`。程序不会自动安装服务、计划任务、hooks、
更新检查或遥测。

在 macOS/Linux 的仓库根目录交叉编译：

```sh
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 GOTOOLCHAIN=go1.25.13 \
  go build -trimpath -o /tmp/agent-deck-native.exe ./cmd/agent-deck-native
```

在 Windows 已安装匹配版本 Go 的环境中构建：

```powershell
$env:CGO_ENABLED = '0'
go build -trimpath -o .\agent-deck-native.exe .\cmd\agent-deck-native
.\agent-deck-native.exe --version
```

非 Windows 平台可以编译和测试参数解析，但运行命名管道或 ConPTY 会明确报错。

## 多会话列表

在第一个 PowerShell 终端运行管理进程：

```powershell
.\agent-deck-native.exe serve
```

默认注册目录为 `%LOCALAPPDATA%\AgentDeckNativeManager`，仅当前用户和 SYSTEM
可以访问。每个专用目录只有一个管理进程持有注册库；默认命名管道根据用户和目录
生成。测试或独立实例可显式指定 `--state-dir` 和 `--pipe`，所有客户端需使用同样的值。
自选目录必须是已有可信父目录下的专用目录。

在第二个 Windows Terminal 窗口进入列表：

```powershell
.\agent-deck-native.exe ui --cwd 'C:\Projects\demo'
# 不带参数也会进入同一列表。
.\agent-deck-native.exe
```

| 按键 | 操作 |
|---|---|
| 上/下或 j/k | 选择会话 |
| n | 输入名称、目录，选择 Shell 或 Codex 后创建并进入 |
| Enter 或 o | 打开；已停止或退出的会话会显式启动 |
| Ctrl+Q | 从会话终端返回列表，后台进程继续运行 |
| s | 确认后停止所选进程树，保留记录 |
| r | 修改名称 |
| d | 确认后停止所选进程树并移除列表记录，保留项目文件 |
| q 或 Ctrl+C | 退出列表，管理进程和会话继续运行 |

列表显示进程观测状态：正在运行、已退出、已停止或异常；不会把进程运行解释为模型忙碌、
等待授权或已完成任务。列表每秒刷新一次，进入会话终端后暂停列表轮询。带运行标识
的异常实例仍可按 s 精确清理；部分启动后的清理失败不会被报告成已停止或已移除。
Codex 使用真实的原生 `codex.exe`；当前 Windows x64 查找也包含 npm 安装的
`@openai/codex-win32-x64` 路径。其他安装布局可通过 `create` 指定实际程序。

也可以用命令管理注册记录：

```powershell
.\agent-deck-native.exe create --name '工作 Shell' --cwd 'C:\Projects\demo' --tool shell --json
.\agent-deck-native.exe list --json
.\agent-deck-native.exe create --name '指定程序' --tool custom --cwd 'C:\Projects\demo' --no-start -- 'C:\Tools\agent.exe' '--model' 'two words'
.\agent-deck-native.exe start --session '<session UUID>' --json
.\agent-deck-native.exe rename --session '<session UUID>' --name '新名称'
.\agent-deck-native.exe remove --session '<session UUID>'
.\agent-deck-native.exe shutdown
```

`create` 先保存描述再启动。启动失败时仍返回保存的 ID，退出码为失败；列表提示
“会话已创建，启动失败”，选择同一记录重试，不需要再次创建。
注册库只保存名称、工具、目录、独立 argv 和显式 `--env` 覆盖；不保存继承环境、
账户配置或屏幕历史。管理进程重启后记录仍在，所有记录处于已停止状态，
程序不会自动重启 Agent。

管理进程中的 `attach`、`send`、`capture`、`resize` 和 `stop` 使用同一管道，
并增加 `--session UUID`。每次启动分配新的 `--run-id UUID`，旧查看者的输入和
缩放请求无法送到后来启动的进程。交互查看者绑定该标识；脚本可明确指定它，
未指定的单次操作先读取当前标识再提交。

`serve` 持有所有会话。关闭它的终端、系统退出或它意外退出会结束所属进程树。
要在 SSH 登出后保活，需要由独立启动器持有管理进程；程序没有自动安装系统服务。
`shutdown` 清理所属进程树并关闭管理进程，保留注册库和项目文件。

## 手动运行一个 Shell 会话

以下命令均在 PowerShell 中执行。每个会话选取不同的管道名，完整路径必须以
`\\.\pipe\agent-deck-native-` 开头。后缀仅接受字母、数字、下划线和短横线；
不接受远程管道地址。

```powershell
$pipe = '\\.\pipe\agent-deck-native-demo'
.\agent-deck-native.exe host --pipe $pipe --cols 120 --rows 30 -- cmd.exe /d /q
```

`host` 在前台持续运行，启动成功后打印一行 JSON，含 `ready`、`host_pid`
和子进程 `pid`。它持有 ConPTY 和子进程，客户端断开不会结束它。
在第二个终端打开同一个目录后操作：

```powershell
$pipe = '\\.\pipe\agent-deck-native-demo'
.\agent-deck-native.exe status --pipe $pipe --json
.\agent-deck-native.exe send --pipe $pipe --text 'echo hello' --enter
.\agent-deck-native.exe capture --pipe $pipe --screen
.\agent-deck-native.exe resize --pipe $pipe --cols 100 --rows 25
.\agent-deck-native.exe attach --pipe $pipe
```

`attach` 要求输入、输出连接到交互式控制台；重定向或普通 SSH 命令可以用
`send`、`capture` 和 `status`。进入后按 Ctrl+Q 返回调用方终端，仅关闭客户端。
再次执行 `attach` 会恢复当前屏幕和光标，并按后续屏幕快照更新。后台的终端
模拟器统一回答设备查询，客户端不把原始查询转发给第二个终端。窗口尺寸每
250ms 检查一次，屏幕每 40ms 读取一次，有变化才重新绘制。

子进程退出后，host 保留最后的屏幕、有限输出和退出码，直到明确停止：

```powershell
.\agent-deck-native.exe stop --pipe $pipe
```

`stop` 在所属进程树和终端输出清理完成后确认请求，然后关闭这个 host。
`host` 自身的 Ctrl+C、关闭其终端、
系统退出或 host 崩溃也会结束会话。因此通过 SSH 启动时，SSH 登出后是否仍运行，
取决于 host 是否由独立的进程持有；前台启动不能承诺登出保活。验收工具可以显式
创建、检查并移除独立计划任务，程序本身不自动设置这些系统配置。

## 启动 Codex 或指定程序

`--` 后的第一个参数是可执行程序，剩余参数保持为独立 argv。程序不隐式拼接
Shell 命令，也不读取或修改现有 Agent Deck、Codex 或 Claude 配置。

```powershell
$pipe = '\\.\pipe\agent-deck-native-codex'
.\agent-deck-native.exe host --pipe $pipe --cwd 'C:\Projects\demo' -- codex.exe
```

上面的程序名需与本机实际安装匹配。`.cmd` 或 `.bat` 包装器需要显式指定
`cmd.exe`；例如用户已选择运行包装器时可以使用
`-- cmd.exe /d /c codex.cmd`。测试阶段优先使用真实的原生 `.exe` 路径，
带空格的路径在 PowerShell 中用引号包住。

未提供 `--env` 时继承 host 的全部环境。重复 `--env NAME=VALUE` 是覆盖，
不会丢弃 PATH；Windows 环境变量名按大小写不敏感处理。

```powershell
.\agent-deck-native.exe host --pipe $pipe --env 'AD_MARK=中文值' -- cmd.exe /d /q
.\agent-deck-native.exe send --pipe $pipe --text 'echo %AD_MARK%' --enter
```

`send --text` 发送 UTF-8 字节，`--enter` 追加 `\r`。也可以只发送 `--enter`。
需要控制字符或任意二进制字节时用 `--base64`，它与 `--text`、`--enter` 互斥：

```powershell
# Ctrl+C 字节，不是停止 host 的命令。
.\agent-deck-native.exe send --pipe $pipe --base64 'Aw=='
```

每次输入最多 64 KiB，终端尺寸为 1–500 列、1–300 行。
输入写入成功仅表示终端收到请求，不表示 Agent 已完成工作。

## 输出与验收边界

host 在内存中保留最近 4 MiB 原始输出，退出 host 后这些记录消失。
`capture --screen` 输出当前可见屏幕的纯文本，保留的原始输出由
`capture --after OFFSET` 读取。`capture --json` 返回一次最多 256 KiB
原始输出（JSON 中为 Base64），并包含以下位置字段：

| 字段 | 含义 |
|---|---|
| `start_offset` | 本次返回数据的起点 |
| `end_offset` | 本次返回数据之后的位置，下次增量读取使用它 |
| `head_offset` | 当前终端屏幕对应的全部已读取输出位置 |
| `truncated` | 请求位置早于内存保留范围，历史已被截断 |

默认的非 JSON `capture` 自动读取到首次请求时的输出位置。
输出历史截断会明确提示；`attach` 此时重新恢复当前屏幕，而不会假装恢复完整历史。

本阶段验收需要分别记录以下证据：

1. 在 Windows 原生系统实际创建 ConPTY，并启动 Shell 和真实 Codex 程序。
2. 输入 ASCII、中文、Enter 和控制字符，独立读取实际输出与当前屏幕。
3. 缩放后程序能读到新尺寸；子进程身份保持一致。
4. 关闭多个客户端后，host 和子进程继续运行；重新连接读到原会话输出。
5. 由显式独立 host 启动器运行时，SSH 登出后再登录确认 host 与子进程 PID。
6. 在可见的 Windows Terminal 内验证中文、光标、实时输入、窗口缩放和
   Ctrl+Q 断开后再接回。
7. 停止后检查该 host、子进程、管道和验收使用的任务确实结束。

交叉编译、命名管道应答或 SSH 下输出读取不能代替第 6 项的可见界面验收。
ConPTY API 存在也不能代替真实创建和交互证据。

它没有崩溃后自动恢复、完整滚动历史、查看者独占、
完整终端模式恢复或多窗口尺寸协调。多个客户端可以同时发送输入，最后一个
尺寸请求决定终端尺寸。颜色、中文、全屏界面和终端设备查询需要真机验证；
host 崩溃后的恢复与现有完整 TUI 集成属于后续阶段。

本机单元验证使用仓库规定的隔离入口：

```sh
scripts/run-isolated.py go test ./cmd/agent-deck-native ./internal/conpty ./internal/winipc ./internal/terminalhost ./internal/nativeregistry ./internal/nativefleet ./internal/nativeui
```

## Windows 自动验收工具

`scripts/windows-native-smoke.ps1` 显式建立一个独立的临时计划任务，在测试结束时
停止并移除该任务。它使用专用临时工作目录，不修改现有 Agent 配置。

```powershell
.\scripts\windows-native-smoke.ps1 -BinaryPath .\agent-deck-native.exe -ReportPath .\native-report.json
# 可选：验证真实 Codex 程序的版本和启动屏幕，使用独立 CODEX_HOME，不提交模型消息。
.\scripts\windows-native-smoke.ps1 -BinaryPath .\agent-deck-native.exe -CodexPath 'C:\Tools\codex.exe'
```

`-StartOnly` 只启动 host，返回精确的 `pipe`、`task_name`、`host_pid` 和 `child_pid`，
供关闭 SSH 后用另一条连接核对同一会话。它保留临时任务，执行时间上限为五分钟。
测试者完成核对后，用返回的管道执行 `stop`，并用返回的任务名精确停止和移除任务。
此模式的 `passed` 仅表示启动成功，不表示完整验收通过。

第二阶段使用独立的多会话验收工具：

```powershell
.\scripts\windows-native-fleet-smoke.ps1 -BinaryPath .\agent-deck-native.exe -ReportPath .\fleet-report.json
```

它创建当前 SID 下无触发器、十分钟时限的临时任务，验证两个 Shell、独立输出、
选择性停止、重启标识、旧输入拒绝、改名与仅删除描述、所属进程清理，以及同一注册库
恢复为已停止记录。测试结束精确移除本次任务，保留独立目录用于诊断。
其 `-StartOnly` 返回管理进程和两个会话的精确标识，供另一条 SSH 连接验证保活；
调用者需按返回的管道执行 `shutdown`，再停止和移除返回的任务。

## 2026-10-13 原生验证记录

目标系统为 Windows Server 2025 Standard，x64，Build 26100。

- Windows ConPTY 全套测试连续三次通过：真实 argv/env/cwd、中文与 emoji、输入、
  实际控制台尺寸、最终输出和退出码、所属子孙清理、无关进程保持、宿主意外退出，
  以及四个启动失败阶段和真实无效 PE 映像的资源回收。资源检查先预热 Go 运行时，
  再要求句柄基线和直接子进程数量准确恢复。
- Windows Named Pipe 测试通过：本机 roundtrip、用户 DACL、重复监听拒绝、连接取消、
  路径别名拒绝，以及目录 ACL 不改动已有子文件、新子文件正确继承的原生回读。
- 真实 CLI 在第二个 ConPTY 控制台中通过中文交互、DA/DSR/光标查询各回答一次、
  Ctrl+Q、原 host/root PID 保持、接回原屏幕、原生 123×35 缩放和停止清理。
- 独立计划任务中的 Shell 通过中文输出、132×42 原生尺寸、多个客户端重新连接。
  真实 Codex 0.154.0 的版本输出和启动画面均通过 ConPTY 捕获，使用独立测试主目录，
  没有提交模型消息。
- 第一条 SSH 连接结束后，第二条连接确认同一 host/root PID 仍运行并执行新命令；
  第三条连接确认停止、所属进程退出和临时任务移除。
- 本机上述四个 Go 包的隔离 `-race`、Windows amd64 `go vet` 和交叉编译通过。

这些是原生系统和控制台链路证据。可见 Windows Terminal 桌面操作尚未验收；
也未证明已登录 Codex 的真实模型回合、Claude、MCP 或完整 Agent Deck TUI 已移植。

## 第二阶段原生验证记录

同一 Windows Server 2025 目标通过以下验证：

- 注册库全套 Windows 测试：SQLite 持久化、真实子进程独占锁、中文名称、UUID、
  损坏与未来 schema 拒绝、路径别名/硬链接拒绝和用户 DACL 原生回读。
- 最新 CLI 在真实 ConPTY 控制台中操作中文列表：键盘输入名称、目录和工具，创建
  两个 Shell，执行编码命令得到中文标记，Ctrl+Q 返回列表，退出 UI 后保留两个 PID，
  新 UI 接回原屏幕和 PID，确认后停止一个而另一个保持运行。
- 重启分配新 RunID，旧 RunID 输入被拒绝；改名与移除保留项目哨兵文件，管理进程
  重启保留描述且不自动启动。失败的自动启动返回已保存的 ID，避免重复创建。
- 首次默认注册目录可在仅有 LOCALAPPDATA 父目录的环境中成功建立。
- 独立临时计划任务的完整多会话验收通过；关闭管理进程后所属根进程退出，任务
  精确移除；重新打开注册库时记录为已停止、PID 为零，原生进程表无子进程。
- 前一条 SSH 连接退出后，另一条连接确认管理进程和两个 Shell 的相同 PID/RunID，
  在原 Shell 执行新编码命令。默认 Codex 查找路径也启动真实原生程序，使用独立
  CODEX_HOME 并读取启动屏幕，不提交模型消息；随后精确停止并移除该测试描述。
- 管理器全套测试在 Windows 实际执行：并发启动一次、旧代请求拒绝、异常输出仍可
  停止、typed-nil 启动失败、部分清理失败保留记录/独占锁、独立慢停止、断开客户端
  保活、关闭等待应答排空，以及 32 会话/64 客户端上限。七个相关包的隔离 race 和
  Windows amd64 vet 通过。

上述 UI 流程是实际 Windows 控制台链路的自动化证据，仍不代表可见桌面的人工验收。
