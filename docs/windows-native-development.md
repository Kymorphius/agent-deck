# Windows 原生开发与验证

<!-- matrix/hardened: develop and test the native command on Windows while preserving live sessions and user credentials. -->

Windows 专属开发使用独立源码 checkout 和与 `go.mod` 匹配的 Go 工具链。
macOS/Linux 可继续做跨平台回归和交叉编译；Windows 上直接构建、运行 ConPTY
及命名管道测试，验证目标机的实际行为。完整原主程序仍有 Unix 依赖，此流程
针对 `cmd/agent-deck-native`。

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

Windows Terminal、中文输入法和桌面剪贴板的验收需要实际桌面交互。
SSH 下的进程、模型回复、快捷方式读回或 headless ConPTY 测试不能代替这一层。
