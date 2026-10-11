# CLAUDE.md（本仓库是 agent-deck 的自维护加固版）

**先读 `MATRIX-HARDENED.md`**（本机另有不入库的 `MATRIX-LOCAL.md`，也要读），里面有为什么加固、R1–R8 要求、改动地图、编译测试方法、合并上游的流程和验收清单。

最重要的规矩：
- 在分支 `matrix/hardened` 上工作；远程 `upstream` 是上游，`origin` 是我们的公开仓库 Kymorphius/agent-deck。只在用户要求时推送；本机专属内容只写进 `MATRIX-LOCAL.md`（不入库），绝不提交到公开仓库。
- 不得破坏 R1–R8：更新请求不带令牌、默认不检查更新；不装自动更新定时器；没有遥测；不发反馈；钩子只在显式安装时写入；`dangerous_mode` 默认 false。每处改动都写 `matrix/hardened:` 注释。
- matrix/hardened: 跑测试统一用 `scripts/run-isolated.py go test ...` 隔离 HOME 和 XDG、复用 Go 缓存并自动回收临时目录（见文档第 4 节）。禁止裸 `HOME=$(mktemp -d)` 测试命令。跑完检查并清理 launchctl 任务和 tmux 会话：隔离 HOME 挡不住它们。
- 安装只用 `scripts/matrix-install.sh`，装到 `~/.local/bin/agent-deck`。绝不用 Homebrew 装 agent-deck。
- 不要把 `[claude].command` 设成 `claude.exe` 的绝对路径，会绕过本机的启动包装（见 `MATRIX-LOCAL.md`）。
- 用户已明确选择 `claude.dangerous_mode = true`（不问权限），不要改回去。
