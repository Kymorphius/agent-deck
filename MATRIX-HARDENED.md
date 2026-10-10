# matrix/hardened：Agent Deck 自维护加固版交接文档

本仓库是 [asheshgoplani/agent-deck](https://github.com/asheshgoplani/agent-deck) 的**自维护分支**，供维护者自己的 Mac 使用。接手修改前请完整读完本文。

- 分支：`matrix/hardened`（基于上游标签 `v1.16.26`，提交 `34cf369`）
- 远程：`upstream` = 上游仓库；`origin` = [Kymorphius/agent-deck](https://github.com/Kymorphius/agent-deck)（**公开**，推 `matrix/hardened` 和 `main`）。只在用户要求时推送。仓库公开，本机专属内容一律写进不入库的 `MATRIX-LOCAL.md`，推送前检查没有本机路径、网络设置、私人邮箱。
- 安装位置：`~/.local/bin/agent-deck`，版本号 `1.16.26+matrix.11`。由 `scripts/matrix-install.sh` 编译安装。
- **绝不再用 Homebrew 装 agent-deck**（`asheshgoplani/tap`），原因见下文。
- 背景记录：见不入库的 `MATRIX-LOCAL.md`（审计结论与替换过程）。

## 1. 为什么要自维护

2026-10-08 通过 Homebrew 安装原版后做了审计，程序本身可信：与 GitHub 发布逐字节一致，发布来源证明校验通过。但默认行为和本机的隐私要求冲突：

1. 约每 2 分钟请求 `api.github.com/repos/asheshgoplani/agent-deck/releases/latest`，并带上 `Authorization: Bearer <token>`。令牌按 GITHUB_TOKEN → GH_TOKEN → `gh auth token` 的顺序取，实际取到的是 gh 里权限很宽的个人令牌。
2. 自动安装每日更新的 LaunchAgent `com.agentdeck.autoupdate`，从界面、通知守护进程、无人值守更新这几条路径都会重新装回来。
3. 内置 PostHog 遥测（`eu.i.posthog.com`），首次弹出的同意框默认选"分享"。
4. `[claude] dangerous_mode`（跳过权限确认）默认开，新建会话对话框会把它写回配置。
5. 反馈弹窗用 `gh api graphql` 以用户身份把内容公开发到 GitHub Discussion。
6. 在 `~/.claude/settings.json` 装 7 个全局钩子，并会悄悄重装（界面启动、通知守护进程"修复"）；卸载时不清理。

## 2. 加固要求（R1–R8）：修改时不得破坏

| 编号 | 要求 |
|---|---|
| R1 | 默认不做任何更新检查或发布轮询；即使被重新打开，任何更新、发布请求也**永不附带 GitHub 令牌或 Authorization 头**。手动 `agent-deck update` 可以用，但不带认证，并且拒绝用原版发布包覆盖本程序。 |
| R2 | 任何路径都不安装、不重装、不"确保"自动更新的 LaunchAgent 或 systemd 定时器。只保留显式移除（`update --uninstall-timer`）。 |
| R3 | 遥测完全移除：不编译进 PostHog 密钥，发送函数是空操作、不建立网络连接，不显示同意框，忽略已存的同意记录。 |
| R4 | `dangerous_mode` 默认 false。新建会话对话框不会悄悄把 true 存进配置。用户显式设置的值照常生效。 |
| R5 | 反馈弹窗不显示，也绝不通过 gh 或 GitHub 发送任何内容。 |
| R6 | Claude 钩子只在用户显式执行 `agent-deck hooks install`（或本次明确同意）时安装；界面和通知守护进程不再"修复"或重装钩子。`agent-deck uninstall` 和 `uninstall.sh` 只删除真正运行 `agent-deck hook-handler` 的钩子（允许前面带 `VAR=值` 前缀），其它钩子保留，删除前先做带时间戳的备份；改写时保持软链接和文件权限不变。 |
| R7 | 版本号能看出是我们的构建：`<上游版本>+matrix.N`。用 `+` 而不用 `-`，因为 `-` 在版本号规则里表示预发布，会让上游正式版显得更新。 |
| R8 | 改动最小，每处都有 `matrix/hardened:` 注释说明原因；不做无关重构；只修改那些断言了被移除行为的测试。 |

所有改动都能用 `git grep -n 'matrix/hardened'` 找到。

## 3. 改动地图（v1.16.26 → matrix/hardened）

查看完整差异：`git diff v1.16.26..matrix/hardened`。核心文件如下：

**更新与令牌（R1）**
- `internal/update/update.go`：`resolveGitHubToken` 不再使用；任何请求都不附带 Authorization（约 229–244 行）。`guardHardenedSelfReplace` / `ErrHardenedSelfReplace`（约 721–800 行）拒绝用上游发布包覆盖加固版。
- `internal/session/userconfig.go`（约 1505–1625 行）：`check_enabled`、`auto_install`、`auto_update`、`auto_update_remotes`、`manage_timer` 未设置时一律视为 false。
- `internal/session/config_keys.go`：`matrixUpdatesOptIn`。第一次显式 `config set` 打开其中任一键时，先把 `check_enabled` 和 `auto_install` 都置为 false，再应用用户请求的那个键。这样原版写在磁盘上的 true 不会被当成同意，打开一个也不会顺带打开另一个。
- `internal/session/userconfig_merge.go`、`internal/ui/settings_panel.go`：设置面板的默认值与上述一致，并会正确携带 opt-in 标记。
- `internal/ui/update_auto.go`：界面不发起发布轮询（启动时、定时、手动刷新都不发）。
- `internal/ui/remote_version.go`、`cmd/agent-deck/remote_cmd.go`：远程更新时不要求远端安装或修复定时器；`remote update --install-timer` 直接报错。

**自动更新定时器（R2）**
- `internal/session/update_timer.go`：`hardenedNoAutoTimer` 总开关，`AutoEnsureUpdateTimer` 总是跳过。
- `cmd/agent-deck/update_cli.go`、`cmd/agent-deck/main.go`（约 4019–4087 行）：`--install-timer` 和 `--ensure-timer` 被拒绝或变成空操作；无人值守运行时若 `check_enabled` 关闭就直接跳过。
- `cmd/agent-deck/notify_daemon_cmd.go`：通知守护进程不再修复定时器，也不再修复钩子（原 `healClaudeHooksAtDaemonStart` 已删除）。

**遥测（R3）**
- `internal/telemetry/config.go`：`hardenedNoTelemetry`，不接受编译进来的密钥。`internal/telemetry/env.go`：`ReasonHardened`，测试之外始终关闭。`internal/telemetry/send.go`：`post()` 直接返回错误，不联网（`DefaultEndpoint` 常量只剩帮助文字里还在用）。
- `.goreleaser.yml`、`.github/workflows/release.yml`、`Makefile`：不注入 PostHog 密钥；Makefile 固定版本号。
- `cmd/agent-deck/telemetry_cmd.go`：删除卸载时的遥测（`maybeSendUninstallTelemetry`）。
- `internal/ui/home.go`：`hardenedNoPrompts`，从不弹同意框，启动时和每小时都不尝试上传。

**dangerous_mode 与反馈（R4、R5）**
- `internal/session/userconfig.go`（GetDangerousMode 默认 false）、`internal/ui/home.go`（新建会话对话框不悄悄写回）。
- `internal/feedback/sender.go`、`internal/ui/feedback_dialog.go`、`cmd/agent-deck/feedback_cmd.go`：`hardenedNoFeedback`，不调用 gh；`agent-deck feedback` 只打印"已禁用"。界面不自动弹出反馈框。

**钩子（R6）**
- `internal/ui/home.go`：`hardenedNoSilentHooks`，界面启动时不重装钩子；也不写 `~/.cursor/hooks.json`，不调用 `HealUsageFeeds`。
- `internal/session/claude_hooks.go`：`isAgentDeckHookCommand` 的判断规则：先去掉开头的 `VAR=值`，再要求第二个词是 `hook-handler`、程序名的文件名部分是 `agent-deck`。`RemoveClaudeHooks` 会扫描全部事件，并做 UTC 时间戳备份（约 387–414 行）。
- `cmd/agent-deck/main.go`（约 4845、5070 行）：`agent-deck uninstall` 会移除这些钩子。`uninstall.sh` 第 253 行附近的 jq 过滤用的是同一套规则（正则），写回时用 `cat >` 保持软链接和权限。

**版本（R7）**：`cmd/agent-deck/main.go:48` 和 `Makefile` 的 `VERSION ?= 1.16.26+matrix.11`。

**测试**：以 `_hardened_test.go` 结尾的文件、`update_check_disable_test.go`、`update_timer_wiring_test.go` 等是新增或调整过的测试；`testdata/goldens/*` 里的快照已经按加固后的输出更新。

## 4. 编译、测试、安装

```sh
cd ~/333.dev/agent-deck
# 编译安装（版本号取最近的上游标签加 +matrix.N；产物里如果有 PostHog 密钥会拒绝安装）
scripts/matrix-install.sh 1
```

**跑测试时必须隔离**，否则会碰到真实的 `~/.claude`、`~/.config/agent-deck` 等：

```sh
T=$(mktemp -d)
env -i PATH="$PATH" HOME=$T XDG_CONFIG_HOME=$T/.config XDG_DATA_HOME=$T/.local/share \
  XDG_CACHE_HOME=$T/.cache XDG_STATE_HOME=$T/.local/state TZ=UTC GOTOOLCHAIN=go1.25.13 \
  go test ./internal/update/... ./internal/telemetry/... ./internal/feedback/... ./internal/session/... ./cmd/agent-deck/...
```

- 本机 Go 是 1.25.5，`go.mod` 要求 1.25.13，用 `GOTOOLCHAIN=go1.25.13`（会自动下载）。
- 代码规范检查：golangci-lint v2.14.0（Makefile 里固定的版本），要用 `GOTOOLCHAIN=go1.26.0` 运行；当前结果是 0 个问题。
- **已知的、与我们无关的测试失败**（上游 v1.16.26 原版同样失败）：`internal/ui` 的 TestMCPDialogFitsScreenAndNamesItsKey；`cmd/agent-deck` 的 TestCLIGoldens、TestConfigShowEffective、TestRecallHookTrigger、TestRecallSearch_Federated、两个 TestSessionHandoff* 和 TestStartupCreationRollback；`internal/session` 约 80 个，其中个别是受负载影响、时好时坏的测试。判断是不是我们引入的问题，要和上游原版的测试结果对比。
- **⚠️ 隔离 HOME 挡不住 launchd 和 tmux。** 测试会以当前用户身份注册 `launchctl` 任务（曾经把测试版 notify-daemon 注册成了 `com.agentdeck.transition-notifier`），还会留下 tmux 会话。每次跑完测试都要检查并清理：
  ```sh
  launchctl list | grep -i agentdeck          # 不应有 transition-notifier / autoupdate
  launchctl bootout gui/$(id -u)/com.agentdeck.transition-notifier 2>/dev/null
  pkill -f 'agent-deck-channels-bin-[0-9]+/agent-deck-test'
  ps -axo pid,args | grep -E '[t]mux .*(agent-deck-test|new-session.*/T/Test)' | awk '{print $1}' | xargs kill
  ```

## 5. 合并上游新版本

```sh
git fetch upstream --tags
git merge vX.Y.Z            # 在 matrix/hardened 上
```

- 冲突的高发区：`internal/update/update.go`、`internal/session/userconfig.go`、`internal/ui/home.go`、`internal/ui/update_auto.go`、`internal/telemetry/*`、`cmd/agent-deck/main.go`、`uninstall.sh`。
- 解决冲突时保留所有 `matrix/hardened:` 改动。上游如果新增了联网、遥测、钩子、定时器或自动更新相关的代码，要按 R1–R6 补上同样的处理。
- 合并后检查上游有没有新增这几类东西：发往外部的 HTTP 请求（`git grep -nE 'http\.(Get|Post|NewRequest)|net\.Dial'`）、新的遥测或分析服务、新的会自己装上的 LaunchAgent 或钩子、新的会去读 `gh auth token` 的地方。
- 改 `Makefile` 的 `VERSION`，按第 4 节跑测试，再运行 `scripts/matrix-install.sh N`。

## 6. 安装后的验收清单

```sh
agent-deck --version                                   # vX.Y.Z+matrix.N
strings ~/.local/bin/agent-deck | grep -cE 'phc_[A-Za-z0-9]{20}'   # 0
agent-deck hooks status                                # 钩子指向 ~/.local/bin/agent-deck
launchctl list | grep -i agentdeck                     # 无输出
for k in claude.dangerous_mode updates.check_enabled updates.auto_update updates.auto_install; do agent-deck config get $k; done
```

## 7. 本机环境与用户的选择

本机专属的说明（启动 Claude 的网络出口设置、安装时改过的外部配置及备份位置、用户的个人选择）放在不入库的 `MATRIX-LOCAL.md`（已加入 .gitignore），接手前请一并读完。

- **不要做**：用 Homebrew 装 agent-deck；`agent-deck update` 换成上游发布包；把令牌或 `gh` 交给它。

## 8. World Store 任务看板（我们自己加的功能）

2026-10-09 新增。任务只存在 World Store（personal-panel 仓库），agent-deck 不存副本，只在打开看板或按 `r` 时读取，用户逐项确认后才写入。

- **连接方式**：运行 personal-panel 的 `scripts/multica-task-bridge.cjs`（`node`，stdin 一个 JSON 请求），它经 World Store 的本用户私有 Unix socket 访问原生资料。不联网、不带令牌、不后台轮询，不影响 R1–R8。World Store 没运行时看板显示不可用。
- **配置**（默认关闭，`bridge` 为空即不启用）：
  ```toml
  [worldstore]
  bridge = "/path/to/personal-panel/scripts/multica-task-bridge.cjs"
  # node = "node"          # 默认 PATH 中的 node
  # device_id = "..."      # 链接里本机的名字，默认主机名去掉 .local；改了会让旧链接失配
  ```
- **一次性初始化**：`agent-deck tasks init-links --yes`，在 World Store 的「任务」类型上建 `任务｜agent-deck 会话关联` 属性（改 World Store 结构，所以必须显式 `--yes`）。不初始化也能看板和改状态，只是不能关联会话。
- **用法**：TUI 里 `alt+t` 打开看板（打开时光标所在的会话就是“当前会话”）：`h/l j/k` 移动，`m` 改状态（待办/进行中/暂停/已取消），`x` 完成，`L/U` 关联/解除当前会话，`n` 在当前会话的目录和分组里新建 claude 会话并把任务作为首条消息、自动关联，`Enter` 跳到已关联会话，`/` 筛选，`r` 重新读取。CLI：`agent-deck tasks [list|show|move|done|link|unlink|launch|init-links] --help`。
- **写入安全**：每次写都带读取时的 owner 和 revision，过期返回 CONFLICT；`WRITE_UNCERTAIN`（超时、中断、回读不符）表示可能已写入，看板锁住所有写操作直到重新读取，绝不自动重试。导入对象只读；“已调整、周期进行中”等非标准状态只展示，状态改动回 World Store 做。
- **改动地图**：`internal/worldstore/`（客户端、分栏、launch）、`internal/ui/kanban_panel.go`、`internal/ui/kanban_home.go`、`cmd/agent-deck/tasks_cmd.go`、`internal/session/userconfig_worldstore.go`（新文件）；对上游文件只加了钩子行：`internal/session/userconfig.go`（`WorldStore` 字段，必须在结构体里，否则 SaveUserConfig 重写 config.toml 时会丢掉这一节）、`internal/ui/home.go`（字段、SetSize、按键路由、View、hasModalVisible、热键分支、updateInner 开头的消息分发，共 7 处）、`internal/ui/hotkeys.go`（`task_board` = `alt+t`）、`cmd/agent-deck/main.go`（`tasks` 分发和 commandRegistry）。合并上游时这些钩子行都带 `matrix/hardened` 注释。
- **personal-panel 侧**：`src/native-task-execution-links.cjs`、`src/multica-task-bridge.cjs` 接受 `source: "agent-deck"`，存进独立属性，与 Codex 关联互不影响；`src/native-game-snapshot.cjs` 读取该属性。说明见该仓库 `docs/multica-task-bridge.md`。
- **测试**：`go test ./internal/worldstore/ ./internal/ui/ -run Kanban`；personal-panel 里 `npm test`。真实写入（改状态、关联、新建会话）没有在真实空间上做过自动化测试。

## 9. 修过的上游 bug

- **从全局搜索（`G`）接回 Claude 对话后会话立即退出**（`1.16.26+matrix.2`，2026-10-09，上游 main 当时仍未修）。`internal/ui/home.go` 的 `createSessionFromGlobalSearch` 把 `exec claude --resume <id> <权限参数>` 整条存进 `inst.Command`；启动时 `buildClaudeResumeCommand` 把不是 `claude` 的 Command 当作自定义程序，再包一层，结果是 `exec exec claude --resume <id> … --resume <id> …`，不到 1 秒就退出（`spawn_died_fast`）。修法：和 CLI 的 `add --resume-session` 一样，只设置 `ClaudeSessionID`，Command 保持 `claude`，resume、`CLAUDE_CONFIG_DIR` 和权限参数都交给启动路径拼。测试：`internal/session/matrix_global_search_resume_test.go`、`internal/ui/matrix_global_search_resume_test.go`。修复前已经接回、命令被写坏的会话，用 `agent-deck session set <会话> command claude` 修正。
- **从搜索接回 Claude 对话时在错误目录新开了一段空对话**（`1.16.26+matrix.3`，2026-10-09）。全局搜索（`G`）和 `recall open` 用 recall 索引里的 `cwd` 作为恢复目录，而这个 `cwd` 是对话**最后**所在的目录（Claude 会跟随 Bash 工具里的 `cd`；`internal/recall/ingest/ingest.go` 的 `Session` 后写覆盖前写）。`claude --resume <id>` 只能在对话**启动**目录下找到它（`<config>/projects/` 下的项目目录按启动目录命名），找不到时启动路径退回 `--session-id <id>`，结果是在无关目录用同一个 ID 开了一段空对话，还在那个目录的项目目录里留下同 ID 的小文件。修法：新增 `internal/session/matrix_resume_dir.go` 的 `LocateClaudeResumeDir`，在所有已知 Claude 配置目录里找 `<id>.jsonl`，取最大的那份，返回其中编码后等于所在项目目录名的那个 `cwd`（不一定是第一个：从别的目录续上的对话保留着旧目录的记录），找不到这样的 `cwd` 的副本不能恢复，跳过；找不到或启动目录已不存在就报错，不再猜目录。`G`（`createSessionFromGlobalSearch`）和 `recall open`（`recallOpenPlan`）都改用它；对话文件 2 分钟内被写过（`ClaudeConversationLiveWindow`，多半还开在别的终端里）时拒绝接回，避免两个 Claude 进程写同一段对话。测试：`internal/session/matrix_resume_dir_test.go`、`internal/ui/matrix_global_search_resume_test.go`、`cmd/agent-deck/recall_cmd_test.go` 的 `TestRecall_OpenPlansResumeOrStart`（夹具改成真实的项目目录命名和结束于子目录的对话）。修复前误开的会话和留下的小文件要手动清理：`find ~/.claude/projects -name '<id>.jsonl'`，只保留在启动目录对应项目目录里的那份，然后 `agent-deck recall rebuild`。

- **新会话发送首条消息时 tmux 不带 `-u`**（2026-10-09）。`buildClaudeCommandWithMessage` 给新 Claude 会话发首条消息（`launch --message`、任务看板 `n`）的后台片段在窗格里直接跑 `tmux display-message / capture-pane / send-keys -l`，是全仓唯一没带 `-u` 的 tmux 调用（上游 #1867 要求每个 tmux 调用都带）。窗格环境来自 tmux 服务器，agent-deck 从 launchd 或 web 服务启动时没有 UTF-8 区域设置，中文首条消息就可能被当成非 UTF-8。修法：三处都改成 `tmux -u`。测试：`internal/session/matrix_first_message_utf8_test.go`。同一轮也把接管（`session adopt`）的裸 `tmux` 调用改走 `tmux.ExecContext`，见第 12 节。

## 10. 从搜索接回 Codex 线程（我们自己加的功能）

2026-10-09 新增。上游只能从 recall 索引接回 Claude 对话，Codex 线程只能搜、不能接（"searchable but not resumable yet"），除非已有 agent-deck 会话拥有它。现在 `recall open <会话>` 和 TUI 全局搜索（`G`）回车都能接回 Codex 线程：

- **做法**：新建一个 codex 会话，设 `CodexSessionID`，启动时由 `buildCodexCommand` 拼出 `codex resume <线程ID>`。`add` 的 `--resume-session` 也接受 `-c codex`（线程 ID 必须是裸 UUID），并写入 harness 为 `codex` 的权威 recall 关联，之后 `recall open` 变成启动这个会话。TUI 里命中的线程已被某会话占用时，直接跳到那个会话。
- **Codex 主目录必须对上**：`codex resume` 按 ID 在 `CODEX_HOME` 下找线程，与启动目录无关；但 `buildCodexCommand` 在启动用的主目录里找不到 rollout 时，会**悄悄丢掉 ID、开一段新对话**（#756）。所以 `session.LocateCodexResume`（`internal/session/matrix_codex_resume.go`）只在新会话将使用的主目录里找 rollout：索引行的 `profile` 就是 rollout 所在主目录对应的账号（`[profiles.<名>.codex].config_dir`，默认主目录为空），新会话带上同一个 `--account`。找不到就报错，不猜。`add` 本地会话同样先检查。
- **启动目录**：rollout 里最后一条 `turn_context`（或 `session_meta`）的 `cwd`，即线程最后的工作目录，避免 codex 恢复时询问换目录。目录已不存在就报错。
- **和 Codex App 同时开着（无缝接入）**：本机的 codex 客户端（App 和命令行都算）都经 app-server 后台服务（`codex app-server --managed-daemon`）运行，后台服务是对话记录唯一的写入方，并持有 `~/.codex/thread-writer-locks/<ID>.lock`；加载过的对话即使没人连着也一直占着这把锁。所以多个客户端可以同时挂在同一段对话上，消息实时互相可见，**前提是接入时不改任何配置**：只要 `codex resume` 带了覆盖参数（`-c …`、`--model`、`--yolo` 等），codex 就要求独占，显示 "This conversation is open in another app. Close it there and press R"（2026-10-09 实测，codex 0.161 客户端连 0.162 后台服务）。agent-deck 平常每次都会加身份注入 `-c developer_instructions=…`，因此接回的会话在 `CodexOptions` 里打上 `shared_thread` 标记（`session.MarkCodexSharedThread`），`buildCodexCommand` 遇到这个标记只拼 `codex resume <ID>`，不加 yolo、模型、推理强度和身份注入。代价是这类会话没有 agent-deck 的身份说明。`recall open`、`G` 和 `add -c codex --resume-session` 新建的会话都打这个标记。不再像 Claude 那样拒绝"2 分钟内写过"的对话：最初的版本这样做，结果正开在 App 里的对话一律接不上。
- **拒绝的情况**：子代理线程（`thread_source=subagent`，codex 不接受在上面输入，报错里给出父线程 ID）；rollout 文件已删除。
- **改动地图**：`internal/session/matrix_codex_resume.go`（新文件）；`cmd/agent-deck/recall_cmd.go`（`recallOpenCodexPlan`、帮助文字）；`cmd/agent-deck/main.go`（`add --resume-session` 的校验、rollout 检查、关联 harness、flag 说明）；`internal/ui/home.go`（`handleGlobalSearchSelection` 的 Codex 分支、`createCodexSessionFromGlobalSearch`）；`internal/session/tooloptions.go`（`CodexOptions.SharedThread`）；`internal/session/instance.go`（`buildCodexCommand` 的共享对话分支）；goldens `help_add.stderr.golden`、`help_recall.golden`。
- **测试**：`internal/session/matrix_codex_resume_test.go`、`cmd/agent-deck/recall_cmd_test.go` 的 `TestRecall_OpenResumesCodexThread`、`internal/ui/matrix_global_search_resume_test.go` 的 `TestMatrixGlobalSearch_CodexHit`，以及 `TestMatrixCodexSharedThreadResumesWithoutOverrides`（共享对话的启动命令不带覆盖参数）。真机验证（2026-10-09，手动）：用一个 `--remote unix://` 客户端占住一段测试对话、模拟 App 开着，再用单独的 agent-deck 配置 `add -c codex --resume-session` 加 `session start`。agent-deck 启动的是 `codex resume <ID>`，没有锁屏；用 `session send` 发的消息在两边实时出现，只写进同一个记录文件。

## 11. 列表的 vim 操作、Alt+Q 断开（我们自己加的功能）

2026-10-09 新增。会话列表里数字只当 vim 计数（另见下面的行号和 `:N`）：`3j` / `3k`（也可用 ↑/↓、`ctrl+n/p`）一次移动 3 行，计数可多位（`12j`、`10k`，上限 999），没有超时，按其它键作废。

- **跳根分组改成 `'` 加数字**（`'3` 跳到第 3 个根分组）。原来单按 `1`–`9` 跳分组，和计数冲突，用户选了纯 vim。`'` 后面不是 1–9 时，`'` 作废，那个键照常生效。分组标题上显示的数字不变，含义变成 `'N`。
- **`0`**：有计数时追加一位（`10j`）；没有计数时照旧清除状态筛选。`!@#$` 等筛选键不受影响。
- **行号和 `:N`**：列表每一行左边显示行号（vim 的 `:set number`），分组标题也编号，视图模式的分隔线不编号。按 `:` 在底部帮助栏位置打开命令行，`:12` 回车跳到第 12 行（行号超出范围时报错、光标不动），`:set nonu` / `:set nu` 隐藏或显示行号（只在本次运行有效，不写配置），`Esc` 或空命令行上的退格取消。行号只在普通列表里画；内嵌侧栏（embedded layout）的卡片高度随宽度变，为了不影响鼠标点击和滚动，那里不画行号，但 `:N` 照样能用。各行渲染时宽度减去行号列，再在前面补行号。
- **`''` 跳回**：回到上一次 `'N` 或 `:N` 跳转之前所在的行，再按一次又跳回去（和 vim 一样来回切）。只有这两种跳转会记位置，`j/k`、翻页不记；跳转没让光标动（比如 `'9` 而没有第 9 组）也不记。记的是那一行的会话或分组，列表折叠、刷新后照样找得到；窗口行和远程行按位置找。和上游的 `` ` ``（直接进入上一个会话）不同，`''` 只移光标。
- **Alt+Q 断开会话**：进入会话后，除了 Ctrl+Q（或配置的 detach 键），Alt+Q 也能回到列表，本地会话和远程（ssh/mosh）会话都行。终端发 Alt+Q 的方式不同：Option 设为 Esc+ 时是 `ESC q`，增强键盘协议下是 `ESC[27;3;113~` / `ESC[113;3u`，iTerm2 默认（Option 为 Normal）时是字符 `œ`，这几种都认。只有这一次读取正好是这个按键时才断开：先按 Esc 再按 q 是两次读取，粘贴含 `œ` 的文字是括号粘贴、按 32 字节分块读，都不会误断开。代价是在会话里打不出单个 `œ`。内嵌侧栏模式（embedded layout）走 Bubble Tea 的按键，那里没加。代码：`internal/tmux/matrix_alt_detach.go`（`MatrixIsAltQDetach`），挂在 `internal/tmux/pty.go` 的 `resolveAttachInterrupt` 开头和 `internal/session/ssh.go` 的 `sshAttachInput.forward` 开头。
- **改动地图**：`internal/ui/matrix_nav_count.go`（新文件，`matrixHandleNavPrefix`）、`internal/ui/matrix_line_numbers.go`（新文件，行号列和 `:` 命令行）；`internal/ui/home.go`（`navCount` 字段、`Update` 里紧跟 jump mode 的 `:` 命令行路由、`handleMainKey` 在 switch 前调用前缀处理、`up/k` 与 `down/j` 按计数循环、`renderSessionList` 的行号列、`View` 里帮助栏替换；原 `1`–`9` 分支保留但已不可达，注释说明）；`internal/ui/help.go`（帮助里 `'1-'9`、`3j / 3k`、`:12`、`''`、`Alt+Q`）。合并上游时注意上游若改了数字键、`'`、`:` 键的绑定或 `renderSessionList` 的循环。
- **测试**：`internal/ui/matrix_nav_count_test.go`、`internal/tmux/matrix_alt_detach_test.go`、`internal/session/matrix_alt_detach_test.go`；`group_nav_test.go` 的 `TestGroupNav_Regression_Plain1_JumpsToFirstRootGroup` 改为按 `'1`。列表快照（`testdata/funccheck/`、`testdata/viewers/`、`tree-*` 帧）已用 `UPDATE_GOLDEN=1` 重新生成，去掉行号列后与原快照逐行一致。

## 12. 接管已在运行的 Claude（我们自己加的功能）

2026-10-09 新增。用户自己在 tmux 里开的 Claude（不是 agent-deck 建的会话）可以不中断地交给 agent-deck 管：`agent-deck session adopt <tmux会话>`，`--list` 列出候选；远程用 `agent-deck remote <名> session adopt …`；TUI 里 `alt+u` 打开列表（光标在远程分组或会话上时列远程的），回车后按 `y` 确认才接管。

- **怎么找**：Claude Code 每个进程在 `<配置目录>/sessions/<pid>.json` 写一个状态文件（`sessionId`、`cwd`、`kind`、`status`）。扫描默认 tmux 服务器和 `[tmux].socket_name` 的服务器（`--socket` 可指定），用 `ps` 的父子关系把 Claude 进程对到窗格。登记的目录用 `LocateClaudeResumeDir` 找到的恢复目录，找不到才用状态文件里的 `cwd`。
- **怎么接**：登记一个 claude 会话（绑定对话 ID、`TmuxSocketName` 指向原服务器），再把 tmux 会话改名成新记录要的名字；进程不动。先改名、后保存，保存失败就改回原名。和 `add` 一样持有注册锁。
- **拒绝的情况**：已被 agent-deck 管（同名同服务器的 tmux 会话，或同一对话 ID 已登记）；tmux 会话里不止这一个窗格（agent-deck 管整个 tmux 会话）；同一对话还有别的活着的 Claude 进程（两个进程写一段对话会写乱）；目录不存在；目标名字在该服务器上已被占用。
- **注意**：接管后在 agent-deck 里删除这个会话会连同 tmux 会话和 Claude 进程一起杀掉；原来的 tmux 会话名没了（`tmux new -A -s 原名` 会新建一个空的）。tmux 列表输出的分隔符必须是可打印字符：没有已连接客户端时（所有经 SSH 的命令）tmux 会把控制字符改成 `_`。
- **tmux 调用**：所有 tmux 命令经 `tmux.ExecContext`（`adoptTmux`），显式 `-L <socket>`（用户自己的服务器是 `default`）。最初版本直接执行 `tmux`，`TestNoRawTmuxExec_OutsideAllowlist` 报错；改用工厂后多了 `-u`（非 UTF-8 环境下中文等非 ASCII 会话名不会被印成 `_`，否则按名字匹配会失败）和测试时不碰真实默认服务器的保护。
- **改动地图**：`internal/session/matrix_adopt.go`、`cmd/agent-deck/matrix_adopt_cmd.go`、`internal/ui/matrix_adopt_panel.go`、`internal/ui/matrix_adopt_home.go`（新文件）；`cmd/agent-deck/session_cmd.go`（`adopt` 分发和帮助）、`cmd/agent-deck/remote_exec.go`（允许转发 `session adopt`）、`internal/ui/hotkeys.go`（`adopt_panel` = `alt+u`）、`internal/ui/help.go`、`internal/ui/home.go`（字段、消息分发、SetSize、按键路由、hasModalVisible、热键分支、View，共 7 处）。
- **测试**：`cmd/agent-deck/matrix_adopt_cmd_test.go`（真 tmux、隔离 socket、`sleep` 代替 Claude、手写状态文件；验证接管后窗格 pid 不变、同一对话重复打开时拒绝、多窗格拒绝）、`internal/ui/matrix_adopt_panel_test.go`。

## 13. 用 `M` 移动分组（我们自己加的功能）

2026-10-09 新增。上游 TUI 里 `M` 只能移动会话，光标在分组行上时按 `M` 没有反应；`K`/`J` 只能和同一层的分组交换顺序。把分组挪到别的父分组下只能用命令行 `agent-deck group change`。现在光标在本地分组行上按 `M`，会打开同一个移动对话框（标题 "Move Group"），选择新的父分组，或者 `(root)` 移到最顶层，回车确认。子分组和组内会话随它一起移动，分组自己的展开/折叠状态保持不变；目标父分组如果是折叠的会被展开，让光标能停在移动后的分组上。

- **引擎**：`GroupTree.MoveGroupTo`，和 `group change` 用的是同一个。
- **候选列表**：`(root)` 排第一，其余按树的顺序排列；不包括分组自己、它的子孙，以及它当前所在的父分组。目标位置已有同名分组时不列出（`MoveGroupTo` 也会拒绝）。有分组范围（scope）时不提供 `(root)`：移到最顶层就出了范围。
- **拒绝的情况**：默认分组（`my-sessions`）；没有可去的地方；远程分组（给出提示，让用户在远端运行 `agent-deck group change`）。
- **重载竞态**：和改名一样登记在 `pendingGroupOps`（`groupOpMoveGroup`），保存失败后重载时补做；源分组不在了，或已经在目标下面，就不做。
- **改动地图**：`internal/ui/matrix_group_move.go`（新文件）；`internal/ui/group_dialog.go`（`moveGroupSource`、`ShowMoveGroup`、`SelectedMoveGroupParent`、标题和 `(root)` 显示）；`internal/ui/home.go`（`groupOpMoveGroup` 常量和补做分支、`M` 的分组和远程分组分支、对话框回车的分组移动分支、分组行底栏的 `M Move` 提示）；`internal/ui/help.go`（GROUPS 里的 `M`）。
- **测试**：`internal/ui/matrix_group_move_test.go`。

## 14. 查看和控制本机的 `agent-deck web` 服务（我们自己加的功能，CLI 部分）

2026-10-09 新增。手机用的 `agent-deck web --no-tui` 是用户自己写的 LaunchAgent / systemd 单元，agent-deck 不安装它。`agent-deck webservice [status|start|stop|restart] [--json]` 读取它在服务管理器里的状态、探测 `<url>/healthz`，并让服务管理器启动、停止或重启它。`remote_exec.go` 放行了 `webservice`，所以也能对远程主机执行。

- **配置**（默认关闭，`manager` 和 `service` 都填了才启用）：
  ```toml
  [webservice]
  manager = "launchd"              # launchd | systemd | systemd-user
  service = "<launchd label 或 systemd 单元名>"
  url = "http://<地址>:8420"       # 可选：探测 /healthz
  # plist = "..."                  # launchd：默认 ~/Library/LaunchAgents/<service>.plist
  # sudo = true                    # systemd：用 `sudo -n systemctl` 控制
  ```
- **行为**：launchd 的 `stop` 是 `bootout`（卸载，避免 KeepAlive 立刻拉起），`start` 在未加载时用 plist `bootstrap`，`restart` 是 `kickstart -k`；操作后最多等 10 秒，直到状态反映出来。健康探测是一次不带凭据的 GET，不走环境里的代理、不跟随重定向（R1/R3 不受影响）。
- **还没做**：TUI 里的 `alt+w` 面板（代码注释里提到它，但还没有接线）；没有自动化测试。2026-10-09 在本机对 `matrix.agent-deck-web` 手动验证过 `status`（运行中、healthz 应答），没有在真机上试 `start/stop/restart`。
- **改动地图**：`internal/session/matrix_webservice.go`（新文件）、`cmd/agent-deck/matrix_webservice_cmd.go`（新文件）；`cmd/agent-deck/main.go`（分发和 commandRegistry）、`cmd/agent-deck/remote_exec.go`、`internal/session/userconfig.go`（`WebService` 字段，必须在结构体里，否则 SaveUserConfig 会丢掉这一节）。
