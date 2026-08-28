# 技能（Skills）管理（二期）— 设计规格

- 日期：2026-08-28
- 状态：待评审
- 分支：`feat/agent-skill-manage`（worktree `.worktrees/agent-skill-manage`）
- 前置：MCP 服务管理一期已合并 main（`a2cdce1`），本 spec 复用其「设置页子区 + 全局管理 + REST + 手动添加」的产品形态。

## 1. Context（为什么做）

一期交付了 MCP 服务管理（连接层），二期补**技能层**：让开发者从 skills.sh 搜索/安装/查看/启用/禁用/删除 agent 技能（Anthropic Agent Skills，`SKILL.md` 格式），并让 dsh agent 真正按技能行事。

调研结论（决定架构）：

- **dsh 内建完整 Agent Skills 系统**（`@deepseek-ai/dsh-skill` / `dsh-skill-filesystem` / `dsh-tool-skill` 三个插件，spine 的 `skills:` 配置，当前 `enabled: false`）：
  - 加载 `<root>/<skill-dir>/SKILL.md`（YAML frontmatter `name`+`description` 必填，name 须 kebab-case `^[a-z0-9]+(?:-[a-z0-9]+)*$`；未知 frontmatter 键忽略）——**与 skills.sh 的技能格式天然兼容**。
  - 模型侧三通道：`skill` 工具（按需加载全文）、每步注入技能目录（`<available_skills>` 清单，变更自动重发）、用户消息 `/技能名` 手势。
  - **chokidar 监听技能根目录**：增删改技能文件下一轮即生效（热）。`enabled` 本身是 init-time（改 cordis 需 respawn）。
  - `customSkillDirs` 配置项指定额外根目录（相对 cwd 解析，用绝对路径注入）。
  - 无 fs 服务也能跑（Node fs fallback）；附属文件（scripts/reference.md）模型暂无工具读取（一期不挂 `dsh-tool-fs`，文件先落盘就位）。
- **skills.sh 链路已实测**：搜索 `GET https://www.skills.sh/api/search?q=<kw>` → `{skills:[{id:"owner/repo/skillName", name, installs, source}]}`；id 即 GitHub 定位 `owner/repo` + 技能目录 `skills/<name>/`（anthropics/skills 与 github/awesome-copilot 两种源均验证为此布局）；`raw.githubusercontent.com/<owner>/<repo>/main/skills/<name>/SKILL.md` 可达。skillhub.cn 无公开 API，排除。

用户决策：完整安装（SKILL.md + 附属文件）；搜索源 skills.sh + 支持手动指定 GitHub 路径；GitHub 拉取用 **tarball 方式**（codeload 单请求、无 API 限流；默认分支 main 失败回退 master）。

## 2. Goals / Non-Goals

**Goals**
- dsh 边车启用 skills 插件（cordis 基模板一次性改动），技能根目录经 env 注入。
- 从 skills.sh 搜索（后端代理）+ 一键安装（GitHub tarball 完整落盘）+ 手动指定 `owner/repo/skill` 安装。
- 已装技能的查看（SKILL.md 内容）/ 启用 / 禁用 / 删除，**全部热生效**（文件操作 + watcher，零 RPC、零边车补丁）。
- 内置校验：name kebab-case、目录名与 frontmatter name 一致、装前预检 SKILL.md 可解析。

**Non-Goals**
- 模型读附属文件的 fs 工具（`dsh-tool-fs` 未挂载，有安全面；文件落盘就位，将来再议）。
- skillhub.cn（无公开 API）。
- 按用户隔离的技能集（延续一期全局）。
- 技能版本管理/更新检测（装的是拉取时刻的快照）。

## 3. 架构总览

```
设置页「技能」子区 ──REST──▶ /api/agent/skills*（Go）
     │ 搜索                        │
     │                             ├─▶ skills.sh /api/search（后端代理，超时 10s）
     └─ 安装/启禁/删 = 文件操作      ├─▶ 安装：codeload.github.com tarball → 解出 skills/<name>/ → 落盘
                                   └─▶ 启禁：enabled/ ↔ disabled/ 目录 rename（chokidar 热生效）
dsh spine（skills 插件常开）── 监听 ZW_AGENT_SKILL_DIR=…/agent-skills/enabled/ → skill 工具 + 目录注入
```

**关键简化（相对一期 MCP）**：skills 插件常开（空目录=无技能），管理动作全部是文件系统操作，dsh 的 watcher 天然热生效——**无需 mcp/apply 式补丁、无需 respawn、无降级路径**。唯一 init-time 项（cordis 基模板 `enabled: true`）是静态文件改动，进程重启后统一生效。

## 4. dsh 边车配置（一次性）

`services/agent-sidecar/cordis.agent.yml` agent-spine 块：
```yaml
    skills:
      enabled: true
      filesystem:
        customSkillDirs:
          - !!js process.env.ZW_AGENT_SKILL_DIR ?? './.dsh/skills-fallback'
```
- 绝对路径经 env 注入（对齐 `ZW_AGENT_MCP_URL` 模式）：`runtime.go` 的 `dshEnv()` 追加 `ZW_AGENT_SKILL_DIR=<skillRoot>/enabled`。
- **注意（e2e 实证修订）**：
  - `skillRoot` 必须**绝对化**（dsh 子进程 cwd=sidecarDir，相对路径相对它再解析而失效——同 CordisConfig 的坑）。
  - `includeDefaultRoots` 必须为 **false**：默认 true 时 dsh 会扫到本机 `~/.agents/skills` 等目录的技能，绕过管理界面泄漏给模型（管理的=可见的全集）。
  - `DSH_HOME` 已设为 `<sidecar>/.dsh`；默认 roots 关闭后只剩 customSkillDirs 一个来源。
- 运行中的旧进程（未启 skills 插件）自然不加载技能；下轮 respawn 后统一生效——不为此做强制 evict（部署重启本来就会发生；一期合并后首次部署即全量生效）。

## 5. 存储布局与数据模型

### 5.1 磁盘（`ZW_AGENT_SKILL_ROOT`，默认 `./data/agent-skills`，gitignore）
```
data/agent-skills/
  enabled/<name>/SKILL.md + 附属文件   ← ZW_AGENT_SKILL_DIR 指这里（dsh 监听根）
  disabled/<name>/...                  ← 移出监听根 = 模型不可见
  .tmp-install-<rand>/                 ← 安装暂存（解 tar → 校验 → 原子 rename 到 enabled/）
```
- 安装原子性：先解到 `.tmp-install-*`，校验通过后 `os.Rename` 进 `enabled/<name>/`；失败清理暂存。
- 启禁 = `enabled/<name>` ↔ `disabled/<name>` rename；删除 = `os.RemoveAll` + 删 DB 行。

### 5.2 表 `agent_skill`（迁移 `000023_agent_skill`）
```sql
CREATE TABLE agent_skill (
  id          BIGINT UNSIGNED NOT NULL,
  name        VARCHAR(64)  NOT NULL,   -- dsh 技能名（=frontmatter name=目录名，kebab-case）；唯一
  display_name VARCHAR(128) NOT NULL DEFAULT '',
  source      VARCHAR(255) NOT NULL DEFAULT '', -- 'owner/repo/skill'（skills.sh id）或 'manual'
  description TEXT NOT NULL,                     -- frontmatter description（列表展示）
  enabled     TINYINT(1) NOT NULL DEFAULT 1,
  content     MEDIUMTEXT NOT NULL,               -- SKILL.md 全文（查看用，免读盘）
  installed_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_agent_skill_name (name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```
Repo：`internal/repo/agent_skill.go`，模式仿 `MCPServerRepo`（List/Get/Create/SetEnabled/Delete，无 builtin 概念；`SetEnabled` 同步做磁盘 rename——repo 只管 DB，rename 放 service 层，见 §6）。

## 6. 安装器（`internal/agent/skillinstall.go`）

`Install(ctx, source string) (*repo.AgentSkill, error)`，source 形如 `owner/repo/skill`（手动安装同格式）：

1. 解析 source → owner/repo/skillName；校验各段非空、skillName 匹配 kebab-case。
2. 拉 tarball：`GET https://codeload.github.com/<owner>/<repo>/tar.gz/refs/heads/main`，404 回退 `master`（Go `archive/tar` + `compress/gzip` 流式解；http.Client 超时 60s）。
3. 流中找 `*/skills/<skillName>/` 前缀的条目（tarball 根是 `<repo>-<ref>/`），逐个写入 `.tmp-install-<rand>/<skillName>/`（拒绝路径穿越：`filepath.Clean` + 前缀校验；跳过符号链接条目）。
4. 校验：`SKILL.md` 存在、frontmatter `name`+`description` 非空、name == skillName == 目录名、name kebab-case。不合规 → 报错清理。
5. 落库（content=SKILL.md 全文，enabled=1）→ `os.Rename` 暂存目录 → `enabled/<name>/`。
6. 冲突：DB 已有同名（enabled 或 disabled 状态）→ 拒绝（「已安装，请先删除」）。

`Search(ctx, q)`：代理 skills.sh `/api/search?q=`（10s 超时；失败返回 502 + 错误信息）。结果原样透传（id/name/installs/source）。

启禁/删除（service 层组合 repo + 磁盘）：
- `SetEnabled(id, enabled)`：DB 行存在 → rename 目录（源不存在=磁盘态丢失 → 重新安装才能启用，返回明确错误）→ 更新 DB。
- `Delete(id)`：删目录（enabled 与 disabled 下都试）→ 删 DB 行。

**事务边界**：磁盘与 DB 无法原子——顺序「先磁盘后 DB」，失败方向：rename 成功但 DB 失败 → 回滚 rename；删除同理。管理操作低频，简单补偿即可。

## 7. API — `/api/agent/skills`（`internal/agent/skill_handlers.go`，走 authGate）

- `GET  /api/agent/skills` → `{skills:[...]}`（DB 列表）。
- `GET  /api/agent/skills/search?q=<kw>` → `{skills:[{id,name,installs,source}]}`（skills.sh 代理）。
- `POST /api/agent/skills/install` `{source}` → 安装（手动安装同一端点，source 即 `owner/repo/skill`）。
- `GET  /api/agent/skills/{id}` → 单技能详情（含 content）。
- `PATCH /api/agent/skills/{id}` `{enabled}` → 启禁。
- `DELETE /api/agent/skills/{id}` → 删除。

## 8. 前端 — 设置页「技能」子区（web/index.html + app.js，仿 MCP 子区）

- **已装列表**：名称（code 样式）+ 描述截断 + 来源 + 启用开关 + 「查看」（展开 SKILL.md 内容）+ 删除。
- **搜索安装**：搜索框 → 在线结果列表（名称/安装量/来源 + 「安装」按钮）；支持直接手填 `owner/repo/skill` 安装。
- 顶部说明：技能安装/启禁**下一轮对话即生效**（无需重启）。
- 暴露 `loadSkills/searchSkills/installSkill/toggleSkill/deleteSkill` + `switchTab('settings')` 联动加载。

## 9. 错误处理与安全

- tarball 条目路径穿越防护（前缀校验 + Clean + 跳过 symlink）。
- 拉取域名白名单：仅 `codeload.github.com` / `www.skills.sh`（不开放任意 URL）。
- name/目录一致性校验杜绝「目录名 A、frontmatter name B」的混乱。
- 技能内容本身是给模型的指令——存在 prompt 注入面（技能来源 GitHub 公开仓库）。一期为开发者自用后台，提示「仅安装可信来源」；不做事前内容审核（记录在案）。
- `content` MEDIUMTEXT 上限 16MB，SKILL.md 远小于此；tarball 单次拉取限制解包总大小（如 64MB）防恶意大仓库。

## 10. 测试

- `skillinstall` 单测：本地 httptest 伪造 codeload tarball（含路径穿越条目、坏 frontmatter、非 skills/<name> 布局等负例）+ 临时目录断言落盘/校验/原子性。
- `Search` 代理：httptest 伪造 skills.sh。
- repo 集成测试（repotest 隔离库）：CRUD + 唯一约束。
- handler 测试：各端点 + 校验分支（复用 injectUser/orchDSN 模式）。
- e2e：dev 起服务 → 安装 `github/awesome-copilot/git-commit`（或 anthropics/skills/pdf）→ 问知微发消息「/git-commit 帮我提交」或「你有哪些技能」→ 断言模型调 `skill` 工具/列出该技能；禁用 → 再问 → 消失。

## 11. 开放问题（实现中定）
- codeload tarball 大小上限的具体值（64MB 起步）。
- dsh catalog 注入对上下文长度的影响（技能多时每步目录消息变长）——默认 `catalogDescriptionMaxLength=500` 已截断，暂不调。
