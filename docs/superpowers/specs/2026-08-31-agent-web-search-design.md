# 知微 Agent 联网搜索 + web_fetch 设计（Phase 2）

- 日期：2026-08-31
- 分支/worktree：`feat/agent-web-search`（基于本地 main `ebe049a`，已含 Phase 1）
- 范围：给知微加**两个常驻 MCP 工具**——`web_search`（联网搜索）与 `web_fetch`（抓取指定 URL 正文）；免 key 抓取优先 + 设置页可填 API key 兜底；配置共用 `agent_config` 表；设置页加搜索配置卡；人设补一句「不确定/时效/外部资料用 web_search/web_fetch 查证」。**不含**设置页统一管理技能/插件/MCP（Phase 3）。
- 关联：Phase 1（行为路由 + 种子门控，已合并 main）；dsh-free-search spike 结论见 §2。

## 1. 目标与决策（已与用户确认）

| 项 | 决策 |
|----|------|
| dsh-free-search | **不采用**：它是完整 `dsh web` 交互式 CLI 的插件（依赖宿主 `dsh-web` 插件的 `searchProvider` + `dsh-tools`/`dsh-settings`/`dsh-client-runtime`），我们的无头 `dsh-sdk-jsonrpc-demo` 边车没有这些。→ **回退原生 Go MCP 工具**。 |
| 搜索后端 | **免 key 优先 + API key 可选兜底**（Q1=a）：先尽力免 key 抓（Bing / DuckDuckGo-lite，失败自动降级下一个），配了 key 走 Tavily。 |
| 配置存储 | **共用 `agent_config` 表**（Q2）：加 `search_engine` + `search_api_key` 两列（新迁移 `000028`）。 |
| 工具形态 | **常驻 MCP 工具**（Q3）：`web_search` / `web_fetch` 常驻可用，模型自主决定何时调用；靠人设引导「常识题自答、不确定/时效性/专业深挖/需外部资料时才搜」。 |
| web_fetch | **常驻**：直接请求指定 URL，返回正文文本（HTML→纯文本），带 SSRF 防护 + 体积/超时上限。 |

## 2. dsh-free-search spike 结论（实证）

审阅 `/tmp/dsh-free-search`（clone）：`package.json` 声明 peerDeps `@deepseek-ai/dsh-settings` + `dsh-tools`；`cordis.patch.yml` 靠 patch 改写宿主 `- id: web, name: '@deepseek-ai/dsh-web'` 的 `searchProvider: ddg` 来接管搜索；`docs/deployment-reproduction.md` 的安装路径全是 `dsh plugin --profile web add` / `dsh --profile web`，设置桥 `/api/dsh-free-search-settings` 靠 client-runtime（web UI）。我们的边车（`services/agent-sidecar/cordis.agent.yml` + `dsh-sdk-jsonrpc-demo` + streamable-http MCP 桥）**无 `web` 插件、无 `dsh-tools`/`dsh-settings`**。→ 采用它需推翻整个 sidecar 架构，不可行。故回退原生 Go 工具（本设计）。

## 3. 架构

新增 `internal/search` 包（仿 `internal/provider` 的单测+实现同包模式），承载搜索引擎与 web_fetch；两个 MCP 工具在 `internal/agent/mcp_tools.go` 的 `registerReadTools` 注册，handler 经 `MCPDeps` 拿到 search provider + `AgentConfigRepo`（每调用读最新配置，因设置页可热改）。

```
用户问题 → dsh 模型判断需外部资料 → 调 mcp__zhiwei__web_search / web_fetch
   web_search: 读 agent_config(engine/api_key) → 选引擎(免key链 / Tavily) → 返回 [{title,url,snippet}]
   web_fetch:  GET url → SSRF 校验 → HTML→文本(截断) → 返回正文
```

**文件职责：**
- `internal/search/search.go`：`Search(ctx, cfg, query, limit)` + 引擎实现（Bing/DDG-lite 免 key；Tavily API key）+ 结果解析。
- `internal/search/fetch.go`：`Fetch(ctx, url)` —— HTTP GET + SSRF 守卫 + HTML→纯文本 + 体积/超时上限。
- `internal/search/{search,fetch}_test.go`：离线单测（httptest 服务器 + HTML 夹具）。
- `internal/agent/mcp_tools.go`：注册 `web_search`、`web_fetch`（`registerReadTools`）。
- `internal/agent/mcp_handlers.go`（或 MCPDeps 定义处）：`MCPDeps` 增加 `Search *search.Searcher`、`Configs *repo.AgentConfigRepo`。
- `migrations/000029_agent_search_config.{up,down}.sql`：`agent_config` 加两列。
- `internal/repo/agent_config.go`：`AgentConfig` 加 `SearchEngine`/`SearchAPIKey`；`Get`/`Upsert` 带上。
- `internal/agent/handlers.go`：`getConfig`/`putConfig` 透出/接收搜索配置。
- `internal/config/config.go`：默认 `DSH_SYSTEM_PROMPT` 追加「不确定/时效/外部资料用 web_search/web_fetch 查证，不要编造」。
- `web/index.html` + `web/app.js`：设置页「知微人设」卡下加「联网搜索配置」卡（引擎下拉 + API key 输入）。
- `cmd/zhiwei-server/main.go`：装配 `search.Searcher` 进 `MCPDeps`。

## 4. web_search 工具

**MCP schema**（`mcp_tools.go`）：
- `web_search`：`query`(必填)、`limit`(可选,默认5,上限10)。返回 `[{title,url,snippet}]` 的 JSON。
- `web_fetch`：`url`(必填)。返回 `{url, title?, text}`（text 截断到 ~固定上限，如 8000 字）。

**引擎选择**（`Search` 按 `cfg.SearchEngine`）：
- `auto`(默认) / `bing` / `duckduckgo`：免 key。按序尝试免 key 引擎，某个失败/空结果自动降级下一个（仿 dsh-free-search 的引擎链）。
- `tavily`：需 `cfg.SearchAPIKey`；`POST https://api.tavily.com/search`。
- 免 key 解析：Bing HTML 结果页 / DuckDuckGo-lite HTML，正则/DOM 粗提 title+url+snippet（**脆弱是已知取舍**，失败即降级；API key 路径稳定）。

**YAGNI**：本期只做 Bing + DuckDuckGo-lite + Tavily 三个后端；SearXNG/Exa/Perplexity 留后续（配置结构预留 `search_engine` 可扩展）。

## 5. web_fetch 工具 + SSRF 防护

- `GET url`（跟随有限重定向），`Content-Type` 非 text/html 直接拒，HTML→纯文本（去 script/style/tag、 collapsing 空白），截断到上限。
- **SSRF 防护（关键）**：解析 URL host → DNS 解析 → **拒绝** 私有/环回/链路本地/保留 IP（`127.0.0.0/8`、`10/8`、`172.16/12`、`192.168/16`、`169.254/16`、`::1`、`fc00::/7` 等）。防止 agent 被诱导请求内网服务（含 `/internal/mcp`）。重定向后再次校验。
- 超时（如 10s）+ 响应体上限（如 2MB 读上限）。

## 6. 数据模型

`migrations/000029_agent_search_config.up.sql`：
```sql
-- 联网搜索配置（Phase 2）：与 identity/soul 共用 agent_config 单例行。
-- search_engine: auto|bing|duckduckgo|tavily（默认 auto=免key链优先）
-- search_api_key: 可选；tavily 等付费后端用，免key后端留空
ALTER TABLE agent_config
  ADD COLUMN search_engine   VARCHAR(32) NOT NULL DEFAULT 'auto',
  ADD COLUMN search_api_key  TEXT         NULL;
```
`.down.sql`：`ALTER TABLE agent_config DROP COLUMN search_api_key, DROP COLUMN search_engine;`
> 注：迁移版本号 000029（main 当前最高 000027 entity_disabled；因与并行分支的 000028_memory_person 撞号，本分支顺延到 000029）。up/down 均提供（repotest 的 iofs 源与 `make init-testdb` 都认 down）。

`internal/repo/agent_config.go`：`AgentConfig` 加 `SearchEngine string \`db:"search_engine"\``、`SearchAPIKey *string \`db:"search_api_key"\``（key 可空用指针）；`Get`/`Upsert` 读写带上（`Upsert` 扩参或单独 `UpsertSearch`）。

## 7. 设置页 + 人设

- **设置页**（`web/index.html`「知微人设」卡后）：加「联网搜索配置」卡——引擎下拉（自动/Bing/DuckDuckGo/Tavily）、API key 输入（password 型，留空=免key）、保存走扩展后的 `PUT /api/agent/config`（指针合并语义：未传字段保持原值）。
- **人设**（`internal/config/config.go` 默认 prompt 末尾追加）：「遇到不确定、时效性、专业深挖或需要外部资料的问题，用 web_search / web_fetch 查证，不要编造。」 → 设置页「整体 prompt」预览自动跟随（它读 `system_prompt`）。

## 8. 测试

- `internal/search`：Bing/DDG HTML 夹具解析→结果；Tavily 走 httptest 假服务器；`auto` 链降级（首选空→下一个）；web_fetch HTML→文本、体积截断、非 HTML 拒、**SSRF 拦截私有/环回 IP**、重定向后再校验。
- `internal/agent`：`web_search`/`web_fetch` MCP handler 返回结构 + 参数校验（query/url 空→tool-error）；config 缺失时降级不崩。
- repo/handler：搜索配置 `Upsert`→`Get` 回读一致；`getConfig`/`putConfig` 透出/接收（指针合并）。
- 迁移：`000028` up/down 应用（repotest 建库即验）。
- 回归：`go test ./internal/agent/... ./internal/search/... ./internal/config/...` 全绿（连 live MySQL）。

## 9. 风险

- **免 key 抓取脆弱**：Bing/DDG 改版会失配——失败自动降级 + API key 兜底缓解；属已知取舍。
- **国内网络**：免 key 直连可能被墙/超时——超时控制 + 降级 + Tavily（境外可达）兜底。
- **SSRF**：必须严格校验解析后 IP（防 agent 打内网/`/internal/mcp`）；重定向后复校。
- **API key 落库**：明文存 `agent_config`（单用户 MVP，与现有 identity/soul 同库同风险级别）；后续可改密钥中心。
- **人设被模型弱化**：同 Phase 1，实测不理想再收敛。

## 10. 不在本期（Phase 3）

设置页统一展示/启禁 技能·插件·MCP 三类（MCP/Skill 已有启禁卡；本期无「插件」类可管）。更多搜索引擎（SearXNG/Exa/Perplexity）。
