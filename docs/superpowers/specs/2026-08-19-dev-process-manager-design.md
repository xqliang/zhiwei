# 调试进程管理脚本设计（dev.sh）

日期：2026-08-19
状态：已批准

## 背景与目标

当前 `make dev` 前台运行 `./bin/zhiwei-server`，占用终端且无法方便地重启。
需要一个脚本，后台管理调试进程的完整生命周期：启动、重启、关闭，附带状态查看与日志跟随。

## 方案选型

| 方案 | 结论 |
|---|---|
| A. 纯 Bash 管理脚本（PID 文件 + 日志文件） | ✅ 采用：零依赖、与 scripts/e2e.sh 风格一致、精确匹配"可控启停"诉求 |
| B. 热重载工具 air | ❌ 不采用：重启时机不受控，与诉求不符；将来可叠加，不冲突 |
| C. 进程管理器（overmind 等） | ❌ 不采用：单进程场景杀鸡用牛刀，MySQL 已由 docker compose 管理 |

## 设计

### 命令接口

```
scripts/dev.sh start     # 预检 → 编译 → 后台启动 → 确认健康检查
scripts/dev.sh stop      # 优雅终止（SIGTERM → 等 5s → SIGKILL）→ 清理 PID 文件
scripts/dev.sh restart   # stop（若在运行）+ start
scripts/dev.sh status    # 存活状态、PID、运行时长、监听端口、最后 5 行日志
scripts/dev.sh logs     # tail -f logs/dev.log
```

无参数调用时打印 usage。

### 文件布局

- `.run/dev.pid`：PID 文件（`.run/` 加入 .gitignore）
- `logs/dev.log`：stdout/stderr 全部重定向至此（`logs/` 加入 .gitignore）
- 日志追加模式，每次 start 写入带时间戳的分隔行；不做自动轮转（开发场景手动清理）

### 启动流程（start）

1. **环境变量预检**：`set -a; source .env; set +a`（.env 不存在时跳过），检查 `ARK_API_KEY`
   非空，缺失则报错退出并提示如何配置
2. **防重复启动**：读取 PID 文件；若进程仍存活（kill -0 校验）则拒绝启动并提示先 stop
3. **编译**：`go build -o bin/zhiwei-server ./cmd/zhiwei-server`（复用 make build 逻辑）
4. **后台启动**：`nohup ./bin/zhiwei-server >> logs/dev.log 2>&1 &`，记录 `$!` 到 PID 文件
5. **健康确认**：轮询 `GET http://localhost:8080/api/health`（最多 5s），
   通过打印 PID 与日志路径；失败则打印日志尾部并自动回滚（stop 掉失败的进程）

### 停止流程（stop）

1. 无 PID 文件 → 提示"未在运行"，正常退出
2. **防 PID 复用误杀**：`ps -p <pid> -o comm=` 校验进程名包含 `zhiwei-server`，
   不匹配则视为陈旧 PID 文件，直接清理退出
3. 发送 SIGTERM，每 0.5s 轮询一次，5s 内退出则成功；超时发 SIGKILL
4. 删除 PID 文件

### Makefile 集成

新增别名：`dev-start` / `dev-stop` / `dev-restart` / `dev-status` / `dev-logs`。

## 错误处理

- 脚本开头 `set -euo pipefail`，出错即停并输出人话错误信息
- 所有子命令对"未运行/已在运行"两种状态幂等友好，不把正常状态当错误刷屏

## 测试计划

- `dev.sh status`（未运行）→ 显示未运行
- `dev.sh start` → 存活、健康检查通过、PID 文件正确
- `dev.sh start`（重复）→ 拒绝
- `dev.sh restart` → PID 变化、服务恢复
- `dev.sh stop` → 进程退出、PID 文件清理；再次 stop → 幂等提示
- 缺 `ARK_API_KEY` 时 start → 明确报错
