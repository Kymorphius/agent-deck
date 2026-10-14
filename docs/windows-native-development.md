# Windows 原生开发与验证

<!-- matrix/hardened: develop and test the native command on Windows while preserving live sessions and user credentials. -->

Windows 专属开发使用独立源码 checkout 和与 `go.mod` 匹配的 Go 工具链。
macOS/Linux 可继续做跨平台回归和交叉编译；Windows 上直接构建、运行 ConPTY
及命名管道测试，验证目标机的实际行为。完整原主程序仍有 Unix 依赖，此流程
针对 `cmd/agent-deck-native`，当前版本为 `0.6.0+matrix.lite`。

目标 Windows Server 2025 已在专用 `AgentDeckNativeDev-*` 目录安装 Go 1.25.13
和 MinGit 2.56.0.windows.2，保留独立源码 checkout、工具及测试产物；没有替换
现有生产二进制、全局 Git/Go 安装或运行中的会话。该环境已完成原生 Go 构建和
离线集成验证，并完成了下文记录的一次限定范围的真实 Codex 编码与测试执行。

## 原生构建

在仓库根目录的 PowerShell 中执行：

```powershell
$env:CGO_ENABLED = '0'
go build -trimpath -o "$env:TEMP\agent-deck-native.exe" ./cmd/agent-deck-native
& "$env:TEMP\agent-deck-native.exe" --version
```

工具安装在独立目录时使用 `go.exe` 的绝对路径即可，不需要修改全局 PATH。
交付前核对 checkout 的提交、二进制版本和 SHA256；编译通过后继续使用原生版
安装器进行显式部署，不用开发产物覆盖运行中的版本。

## 隔离测试

<!-- matrix/hardened: Windows uses the PowerShell runner; the existing Python runner retains its Unix process and tmux ownership rules. -->

Windows PowerShell 5.1 或更新版本使用专用入口：

```powershell
powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass `
  -File .\scripts\windows-native-test.ps1

# 工具未加入 PATH 时，显式指定实际 go.exe。
powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass `
  -File .\scripts\windows-native-test.ps1 -GoPath 'C:\Tools\Go\bin\go.exe' `
  -NativeIntegration
```

默认执行原生相关包的离线测试。`-NativeIntegration` 先从当前 checkout 构建私有
CLI，再启用真实 ConPTY、后台进程及 IPC fixture；模型联网验收不会因此开启。
在 PowerShell 内可通过数组选择包：

```powershell
& .\scripts\windows-native-test.ps1 `
  -Packages @('./internal/nativefleet', './internal/nativestatestore')
```

入口在隔离前解析并复用 Go 的模块缓存、构建缓存和工具链设置，随后清空继承的
进程环境，只保留 Windows 执行所需变量和这些 Go 设置。HOME、USERPROFILE、
APPDATA、LOCALAPPDATA、CODEX_HOME、XDG 与临时目录均指向本次 GUID 私有目录；
原用户的凭据、配置、外部候选 CLI 和在线测试开关不会继承。结束后恢复调用者的
进程环境及工作目录，不更改系统或用户的全局环境。

临时根目录仅当前 SID 与 SYSTEM 可访问。成功时验证目录身份并移除本次目录；
测试失败、目录身份变化、重解析点或清理失败时保留现场、返回非零并报告准确路径。
保留目录供诊断，不按进程名批量关闭后台，也不删除其他测试或用户会话。

## 验收边界

<!-- matrix/hardened: model replies, command execution and desktop visibility are independent evidence. -->

原生构建及离线 fixture 验证代码、ConPTY、进程树和 IPC。真实 Codex 验收还需要
明确选择 CLI 版本、项目及隔离的认证副本，独立确认模型回复、文件修改、测试执行、
保活和重连。桌面版附带的 CLI 与 npm CLI 可能版本不同，分别核对 `--version`；
模型目录列出一个模型也不能代替实际请求成功。

模型能回复并不证明 Windows 命令执行器可用。SSH 非交互会话与已登录的桌面会话
可能表现不同；执行器或沙箱初始化失败时保留错误证据，不自动关闭沙箱或改写用户
配置。后台开发迁移需先验证真实文件操作和 Git 保存。
独立验证必须记录实际执行器版本、进程所在的 Windows 会话、项目文件差异及
测试结果，并保持已登录的其他进程运行。

### 2026-10-14 限定编码验证

官方 Codex `0.163.0-alpha.11` 在已登录的 Windows Session 2 中，使用原生
`gpt-6.1-sol` 及正常的 elevated `workspace-write` 沙箱完成了一次真实编码任务：

- 只新增 `cmd/agent-deck-native/codex_runtime_test.go`，父会话预先提供的
  `codex_runtime.go` 保持不变。六个用例覆盖默认空选择、有效绝对普通文件、相对
  路径、错误文件名、不存在文件及同名目录。
- 工作目录内使用经哈希核对的 Go 1.25.13 windows/amd64 和 MinGit
  2.56.0.windows.2 工具副本，原工具目录、全局 PATH 和用户配置保持不变。
- 实际执行 `scripts/windows-native-test.ps1`，`-GoPath` 指向该 Go 副本，
  `-Packages` 仅包含上述两个 Go 文件。标准库文件列表测试退出码为零，输出为
  `ok command-line-arguments 0.496s`；不需要联网或读取用户凭据。
- 实际执行的 `git diff --check` 退出码为零。工作者没有写入 `.git` 或提交代码，
  父会话负责复核差异及保存 Git 记录；macOS 的三个相关包隔离 race 回归也通过。

这次证据确认真实模型能在 Windows 原生沙箱内修改项目文件并运行有意义的测试，
范围仅为程序选择边界的六个用例。它不代表 0.6 的全模块验证或可见桌面验收已通过。

v0.6 的历史恢复验收还需确认：先保活并接回同一进程，显式停止后按 `h`，在
Codex 历史列表独立核对项目及原对话身份，选择后在同一历史中完成新模型回合。
新的 RunID 应拒绝旧查看者的输入及停止请求，保存的启动描述保持不变。
离线恢复接口测试通过，不能代替这段真实在线流程。

Windows Terminal、中文输入法和桌面剪贴板的验收需要实际桌面交互。
SSH 下的进程、模型回复、快捷方式读回或 headless ConPTY 测试不能代替这一层。

## 并行部署与程序选择

使用原生安装器的 `-StateDirectoryName LiteState-0.6 -ShortcutSuffix 0.6`，可创建
独立的 0.6 列表及入口，保留旧 `LiteState`、旧快捷方式和运行中的后台。发布前
从已提交的源码构建，核对提交身份、`--version`、二进制 SHA256 及快捷方式读回；
不同版本不复用同一状态目录的后台。

如需固定 Codex 程序，安装时同时指定 `-CodexExecutable` 的绝对 `codex.exe` 路径
及 `-ExpectedCodexSHA256`。安装清单和快捷方式保存经核对的哈希，入口启动时
再次验证外部文件；不复制外部程序、登录凭据或修改 PATH。直接运行 UI 时可用
`--codex-executable` 选择程序，但该参数仅检查路径及普通文件身份，不提供安装器
的哈希固定；它只影响随后创建的新 Codex 记录，已有记录继续沿用已保存路径。
完整安装命令见 [Windows 原生精简版](windows-native-prototype.md)。
