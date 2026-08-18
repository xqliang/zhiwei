# 知微云端 MVP 实现计划 · Sprint 0-1（骨架 + 音频 ASR 链路）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 搭起知微 Go 模块化单体骨架，并打通第一条可用链路：Web 录音/文件上传 → ffmpeg 转码 → 火山 Ark ASR（含说话人分离）→ 转写落库 → 时间线页面展示。

**Architecture:** 单个 Go 二进制 `zhiwei-server`（chi + sqlx + MySQL 8），音频上传后由「DB 任务表 + 进程内 Worker 池」异步执行 pipeline stage（本计划到 `segment` 为止，extract/quality/commit 在 Sprint 2 计划中扩展）。全部 AI 调用经 `internal/provider` 接口抽象走火山 Ark。

**Tech Stack:** Go 1.26、chi v5、sqlx + go-sql-driver/mysql、golang-migrate、bwmarrin/snowflake（雪花 ID）、gorilla/websocket（ASR Spike）、ffmpeg（转码）、Vue 3（CDN 产物本地 vendor，无构建）。

**上游 spec:** `docs/superpowers/specs/2026-08-18-zhiwei-cloud-mvp-design.md`

**系列说明:** 本计划 = Sprint 0（Spike + 骨架）+ Sprint 1（音频 + ASR pipeline + 时间线）。Sprint 2（Memory/Todo/Topic）、Sprint 3（检索/Agent）、Sprint 4（Review/打磨）各出独立计划，在本计划验收后编写。

**约定（全计划适用）:**
- 集成测试以环境变量 `TEST_MYSQL_DSN` 是否存在为开关，缺失则 `t.Skip`，不进普通 `make test`
- 所有命令都在仓库根目录执行
- 所有 ID 均为雪花 ID，JSON 里序列化为字符串
- 提交信息结尾带 `Co-Authored-By: Claude <noreply@anthropic.com>`（下文提交步骤中省略，执行时补上）

---

## 文件结构总览

```text
zhiwei-glm53/
├── go.mod / go.sum
├── Makefile
├── .gitignore                          # data/ bin/
├── cmd/
│   ├── zhiwei-server/main.go           # 服务入口
│   ├── spike/llm/main.go               # Ark LLM 真实调用验证（手动）
│   ├── spike/embed/main.go             # Ark Embedding 验证（手动）
│   └── spike/asr/main.go               # Ark ASR 协议验证（手动，Sprint 0 核心）
├── internal/
│   ├── config/config.go                # 环境变量配置
│   ├── ids/ids.go                      # 雪花 ID 类型（JSON 字符串化）
│   ├── repo/
│   │   ├── db.go                       # sqlx 初始化
│   │   ├── session.go                  # audio_session DAO
│   │   ├── job.go                      # pipeline_job DAO
│   │   └── transcript.go               # transcript/segment DAO
│   ├── provider/
│   │   ├── llm.go                      # LLMProvider 接口 + Ark 实现（OpenAI 兼容）
│   │   ├── embed.go                    # EmbeddingProvider 接口 + Ark 实现
│   │   └── asr.go                      # ASRProvider 接口 + Ark 实现（Spike 结论封装）
│   ├── pipeline/
│   │   ├── pool.go                     # Worker 池 + 任务领取循环
│   │   ├── state.go                    # stage 推进/重试纯逻辑（可单测）
│   │   └── stage_asr.go                # asr + segment 两个 stage 实现
│   └── api/
│       ├── router.go                   # 路由装配 + 静态页
│       ├── audio.go                    # POST /api/audio 上传
│       └── query.go                    # sessions 列表/详情、job retry
├── migrations/
│   └── 000001_init.up.sql / .down.sql  # 全量 9 张表（一次到位）
├── deploy/docker-compose.yml           # MySQL 8
├── web/
│   ├── index.html                      # Vue 3 单页（时间线 + 录音）
│   └── vendor/vue.global.prod.js       # 本地 vendor 的 Vue 产物
├── testdata/sample.wav                 # e2e 冒烟用音频
└── data/                               # 运行时音频存储（gitignore）
```

---

### Task 1: 项目骨架与健康检查 API

**Files:**
- Create: `go.mod`、`cmd/zhiwei-server/main.go`、`internal/api/router.go`、`Makefile`、`.gitignore`
- Test: `internal/api/router_test.go`

- [ ] **Step 1: 初始化模块与依赖**

```bash
go mod init zhiwei
go get github.com/go-chi/chi/v5@latest
```

- [ ] **Step 2: 写失败测试（health 接口）**

`internal/api/router_test.go`：

```go
package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealth(t *testing.T) {
	r := NewRouter()
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if got := rec.Body.String(); got != `{"status":"ok"}` {
		t.Fatalf("want ok body, got %s", got)
	}
}
```

- [ ] **Step 3: 运行确认失败**

Run: `go test ./internal/api/`
Expected: FAIL（`NewRouter` 未定义）

- [ ] **Step 4: 最小实现**

`internal/api/router.go`：

```go
// Package api 提供 HTTP 路由装配。MVP 单用户免登录，无认证中间件。
package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// NewRouter 装配全部路由。各业务 handler 通过参数注入，见后续 Task。
func NewRouter() http.Handler {
	r := chi.NewRouter()
	r.Get("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	return r
}
```

`cmd/zhiwei-server/main.go`：

```go
// zhiwei-server 是知微云端 MVP 的唯一入口：HTTP API + 异步 pipeline worker。
package main

import (
	"log"
	"net/http"

	"zhiwei/internal/api"
)

func main() {
	log.Println("zhiwei-server listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", api.NewRouter()))
}
```

`.gitignore`：

```text
data/
bin/
*.log
```

`Makefile`：

```makefile
.PHONY: build dev test test-integration migrate-up migrate-down

build:
	go build -o bin/zhiwei-server ./cmd/zhiwei-server

dev: build
	./bin/zhiwei-server

test:
	go test ./...

# 集成测试：需要 docker compose 里的 MySQL 已启动并完成迁移
test-integration:
	TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3306)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./...

migrate-up:
	migrate -path migrations -database "mysql://zhiwei:zhiwei@tcp(127.0.0.1:3306)/zhiwei" up

migrate-down:
	migrate -path migrations -database "mysql://zhiwei:zhiwei@tcp(127.0.0.1:3306)/zhiwei" down 1
```

- [ ] **Step 5: 运行测试通过**

Run: `go test ./internal/api/ -v`
Expected: PASS

- [ ] **Step 6: 提交**

```bash
git add go.mod go.sum cmd/ internal/api/ Makefile .gitignore
git commit -m "feat: 项目骨架与 /api/health"
```

---

### Task 2: 配置模块

**Files:**
- Create: `internal/config/config.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: 写失败测试**

```go
package config

import "testing"

func TestLoadDefaults(t *testing.T) {
	t.Setenv("ARK_API_KEY", "test-key")
	// 其余走默认值
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Port != "8080" {
		t.Errorf("Port = %s, want 8080", c.Port)
	}
	if c.LLMFastModel != "doubao-seed-1.6-flash" {
		t.Errorf("LLMFastModel = %s", c.LLMFastModel)
	}
	if c.ASRModel != "doubao-seed-asr-2-0" {
		t.Errorf("ASRModel = %s", c.ASRModel)
	}
}

func TestLoadRequiresARKKey(t *testing.T) {
	t.Setenv("ARK_API_KEY", "")
	if _, err := Load(); err == nil {
		t.Fatal("want error when ARK_API_KEY empty")
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/config/`
Expected: FAIL（`Load` 未定义）

- [ ] **Step 3: 实现**

```go
// Package config 从环境变量读取配置，全部有默认值，除 ARK_API_KEY 外可不设置。
package config

import (
	"errors"
	"os"
)

type Config struct {
	Port     string // HTTP 监听端口
	DataDir  string // 音频文件存储目录
	MySQLDSN string // MySQL 连接串

	ARKAPIKey string // 火山方舟 API Key（必填）
	ARKBaseURL string // Ark OpenAI 兼容接口地址
	ASREndpoint string // ASR WebSocket 地址（Spike 后按需调整）

	LLMFastModel   string // Tier1：抽取/分类
	LLMStrongModel string // Tier2：Agent/Review
	EmbedModel     string
	ASRModel       string // Ark 上的 ASR 模型；若账号需要 endpoint 形式（ep-xxx），直接配成 endpoint id
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func Load() (*Config, error) {
	key := os.Getenv("ARK_API_KEY")
	if key == "" {
		return nil, errors.New("ARK_API_KEY 未设置")
	}
	return &Config{
		Port:     getenv("ZW_PORT", "8080"),
		DataDir:  getenv("ZW_DATA_DIR", "./data"),
		MySQLDSN: getenv("ZW_MYSQL_DSN", "zhiwei:zhiwei@tcp(127.0.0.1:3306)/zhiwei?parseTime=true&charset=utf8mb4"),
		ARKAPIKey:  key,
		ARKBaseURL: getenv("ZW_ARK_BASE_URL", "https://ark.cn-beijing.volces.com/api/v3"),
		ASREndpoint: getenv("ZW_ASR_ENDPOINT", "wss://ark.cn-beijing.volces.com/api/v3/asr"),
		LLMFastModel:   getenv("ZW_LLM_FAST", "doubao-seed-1.6-flash"),
		LLMStrongModel: getenv("ZW_LLM_STRONG", "doubao-seed-1.6"),
		EmbedModel:     getenv("ZW_EMBED_MODEL", "doubao-embedding-large"),
		ASRModel:       getenv("ZW_ASR_MODEL", "doubao-seed-asr-2-0"),
	}, nil
}
```

- [ ] **Step 4: 运行测试通过**

Run: `go test ./internal/config/ -v`
Expected: PASS（2 个用例）

- [ ] **Step 5: 提交**

```bash
git add internal/config/
git commit -m "feat: 环境变量配置模块"
```

---

### Task 3: 雪花 ID 类型（JSON 字符串化）

**Files:**
- Create: `internal/ids/ids.go`
- Test: `internal/ids/ids_test.go`

- [ ] **Step 1: 写失败测试**

```go
package ids

import (
	"encoding/json"
	"testing"
)

type payload struct {
	ID ID `json:"id"`
}

func TestMarshalAsString(t *testing.T) {
	b, err := json.Marshal(payload{ID: 1234567890123456789})
	if err != nil {
		t.Fatal(err)
	}
	// 雪花 ID 超过 JS Number.MAX_SAFE_INTEGER，必须序列化为字符串
	if string(b) != `{"id":"1234567890123456789"}` {
		t.Fatalf("got %s", b)
	}
}

func TestUnmarshalFromString(t *testing.T) {
	var p payload
	if err := json.Unmarshal([]byte(`{"id":"1234567890123456789"}`), &p); err != nil {
		t.Fatal(err)
	}
	if int64(p.ID) != 1234567890123456789 {
		t.Fatalf("got %d", int64(p.ID))
	}
}

func TestNewUnique(t *testing.T) {
	if err := Init(1); err != nil {
		t.Fatal(err)
	}
	a, b := New(), New()
	if a == b {
		t.Fatal("生成的 ID 不应重复")
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/ids/`
Expected: FAIL

- [ ] **Step 3: 实现**

先装依赖：

```bash
go get github.com/bwmarrin/snowflake@latest
```

`internal/ids/ids.go`：

```go
// Package ids 提供雪花 ID。雪花 ID 超过 JS 2^53 精度上限，
// 因此 ID 类型在 JSON 中始终序列化为字符串，前后端统一按 string 处理。
package ids

import (
	"strconv"

	"github.com/bwmarrin/snowflake"
)

// ID 是业务主键类型。数据库列 BIGINT，JSON 字符串。
type ID int64

var node *snowflake.Node

// Init 初始化雪花节点。nodeID 取 0-1023，单体单进程固定用 1；
// 未来拆多实例时按服务分配不同 nodeID 即可。
func Init(nodeID int64) error {
	n, err := snowflake.NewNode(nodeID)
	if err != nil {
		return err
	}
	node = n
	return nil
}

// New 生成一个新 ID。Init 未调用时 panic（属于启动装配错误）。
func New() ID {
	return ID(node.Generate().Int64())
}

func (i ID) Int64() int64      { return int64(i) }
func (i ID) String() string    { return strconv.FormatInt(int64(i), 10) }

// MarshalJSON 序列化为 JSON 字符串，规避前端精度丢失。
func (i ID) MarshalJSON() ([]byte, error) {
	return []byte(`"` + i.String() + `"`), nil
}

// UnmarshalJSON 接受带引号字符串或不带引号数字。
func (i *ID) UnmarshalJSON(b []byte) error {
	s := string(b)
	if len(s) >= 2 && s[0] == '"' {
		s = s[1 : len(s)-1]
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}
	*i = ID(v)
	return nil
}
```

- [ ] **Step 4: 运行测试通过**

Run: `go test ./internal/ids/ -v`
Expected: PASS（3 个用例）

- [ ] **Step 5: 提交**

```bash
git add internal/ids/ go.mod go.sum
git commit -m "feat: 雪花 ID 类型，JSON 序列化为字符串"
```

---

### Task 4: Docker Compose + 迁移框架 + 全量 Schema

**Files:**
- Create: `deploy/docker-compose.yml`、`migrations/000001_init.up.sql`、`migrations/000001_init.down.sql`
- Modify: `Makefile`（增加 compose 目标）

- [ ] **Step 1: 写 docker-compose（MySQL 8 + test 库）**

`deploy/docker-compose.yml`：

```yaml
services:
  mysql:
    image: mysql:8.4
    container_name: zhiwei-mysql
    environment:
      MYSQL_ROOT_PASSWORD: root
      MYSQL_USER: zhiwei
      MYSQL_PASSWORD: zhiwei
      MYSQL_DATABASE: zhiwei
    ports:
      - "3306:3306"
    volumes:
      - zhiwei-mysql-data:/var/lib/mysql
    command: --character-set-server=utf8mb4 --collation-server=utf8mb4_unicode_ci

volumes:
  zhiwei-mysql-data:
```

- [ ] **Step 2: 写迁移 SQL（9 张表一次到位，Sprint 2-4 不再加表）**

`migrations/000001_init.up.sql`：

```sql
-- 知微 MVP 全量 schema：雪花 ID 主键，无 AUTO_INCREMENT。
-- 集成测试库 zhiwei_test 由 init-testdb 目标创建。

CREATE TABLE audio_session (
  id BIGINT PRIMARY KEY,
  user_id BIGINT NOT NULL DEFAULT 1,
  source VARCHAR(16) NOT NULL,                -- web_upload | web_record
  filename VARCHAR(512) NOT NULL,
  storage_path VARCHAR(1024) NOT NULL,
  duration_ms BIGINT NOT NULL DEFAULT 0,
  mime VARCHAR(64) NOT NULL DEFAULT '',
  status VARCHAR(16) NOT NULL DEFAULT 'uploaded', -- uploaded|processing|completed|failed
  job_id BIGINT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  KEY idx_user_time (user_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE pipeline_job (
  id BIGINT PRIMARY KEY,
  user_id BIGINT NOT NULL DEFAULT 1,
  session_id BIGINT NOT NULL,
  stage VARCHAR(16) NOT NULL,                 -- asr|segment|extract|quality|commit|done
  status VARCHAR(16) NOT NULL DEFAULT 'pending', -- pending|running|failed|done
  attempt INT NOT NULL DEFAULT 0,
  last_error TEXT NULL,
  trace JSON NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  KEY idx_status_id (status, id),
  KEY idx_session (session_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE transcript (
  id BIGINT PRIMARY KEY,
  user_id BIGINT NOT NULL DEFAULT 1,
  session_id BIGINT NOT NULL,
  language VARCHAR(16) NOT NULL DEFAULT 'zh-CN',
  full_text MEDIUMTEXT NULL,
  confidence DECIMAL(5,4) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  KEY idx_session (session_id),
  KEY idx_user_time (user_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE transcript_segment (
  id BIGINT PRIMARY KEY,
  transcript_id BIGINT NOT NULL,
  sequence_no INT NOT NULL,
  speaker_label VARCHAR(16) NOT NULL DEFAULT '',
  text TEXT NOT NULL,
  start_ms BIGINT NOT NULL DEFAULT 0,
  end_ms BIGINT NOT NULL DEFAULT 0,
  confidence DECIMAL(5,4) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  KEY idx_transcript (transcript_id, sequence_no)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE topic (
  id BIGINT PRIMARY KEY,
  user_id BIGINT NOT NULL DEFAULT 1,
  name VARCHAR(256) NOT NULL,
  description TEXT NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'active', -- suggested|active|dismissed
  created_by VARCHAR(8) NOT NULL DEFAULT 'ai',  -- ai|user
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  KEY idx_user (user_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE memory (
  id BIGINT PRIMARY KEY,
  user_id BIGINT NOT NULL DEFAULT 1,
  type VARCHAR(32) NOT NULL,                  -- event|fact|decision|idea|problem|preference
  title VARCHAR(512) NOT NULL DEFAULT '',
  content TEXT NOT NULL,
  epistemic_type VARCHAR(16) NOT NULL DEFAULT 'observed', -- observed|inferred|suggested
  importance DECIMAL(5,4) NOT NULL DEFAULT 0.5,
  confidence DECIMAL(5,4) NOT NULL DEFAULT 0.8,
  topic_id BIGINT NULL,
  session_id BIGINT NOT NULL,
  transcript_segment_ids JSON NULL,
  event_at DATETIME(3) NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'active', -- active|superseded|dismissed
  embedding LONGBLOB NULL,
  version INT NOT NULL DEFAULT 1,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  KEY idx_user_time (user_id, event_at),
  KEY idx_topic (topic_id),
  KEY idx_session (session_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE todo (
  id BIGINT PRIMARY KEY,
  user_id BIGINT NOT NULL DEFAULT 1,
  title VARCHAR(512) NOT NULL,
  source_memory_id BIGINT NULL,
  topic_id BIGINT NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'suggested', -- suggested|confirmed|done|dismissed
  due_at DATETIME(3) NULL,
  confidence DECIMAL(5,4) NOT NULL DEFAULT 0.8,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  KEY idx_user_status (user_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE daily_review (
  id BIGINT PRIMARY KEY,
  user_id BIGINT NOT NULL DEFAULT 1,
  review_date DATE NOT NULL,
  content JSON NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'pending', -- pending|ready|failed
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_user_date (user_id, review_date)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE agent_message (
  id BIGINT PRIMARY KEY,
  user_id BIGINT NOT NULL DEFAULT 1,
  role VARCHAR(16) NOT NULL,                  -- user|assistant
  content TEXT NOT NULL,
  citations JSON NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  KEY idx_user_time (user_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

`migrations/000001_init.down.sql`：

```sql
DROP TABLE IF EXISTS agent_message;
DROP TABLE IF EXISTS daily_review;
DROP TABLE IF EXISTS todo;
DROP TABLE IF EXISTS memory;
DROP TABLE IF EXISTS topic;
DROP TABLE IF EXISTS transcript_segment;
DROP TABLE IF EXISTS transcript;
DROP TABLE IF EXISTS pipeline_job;
DROP TABLE IF EXISTS audio_session;
```

- [ ] **Step 3: Makefile 增加目标**

在 `Makefile` 追加（保持 `.PHONY` 一行同步更新）：

```makefile
.PHONY: compose-up compose-down init-testdb migrate-test-up

compose-up:
	docker compose -f deploy/docker-compose.yml up -d

compose-down:
	docker compose -f deploy/docker-compose.yml down

# 创建集成测试库并跑迁移
init-testdb:
	docker exec zhiwei-mysql mysql -uroot -proot -e "CREATE DATABASE IF NOT EXISTS zhiwei_test CHARACTER SET utf8mb4;"
	migrate -path migrations -database "mysql://root:root@tcp(127.0.0.1:3306)/zhiwei_test" up

test-integration: init-testdb
	TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3306)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./...
```

（同时删除 Task 1 中旧的 `test-integration` 目标，以本版本为准。）

- [ ] **Step 4: 验证迁移可执行**

前置：安装 migrate CLI（`brew install golang-migrate`）。

```bash
make compose-up && sleep 15
make migrate-up
make init-testdb
```

Expected: 两条 `1/u init` 无报错；`mysql` 库里出现 9 张表。

- [ ] **Step 5: 提交**

```bash
git add deploy/ migrations/ Makefile
git commit -m "feat: MySQL docker-compose 与全量 schema 迁移"
```

---

### Task 5: repo 基础层（sqlx 接入 + 集成测试开关）

**Files:**
- Create: `internal/repo/db.go`、`internal/repo/testutil.go`
- Test: `internal/repo/db_test.go`

- [ ] **Step 1: 装依赖**

```bash
go get github.com/jmoiron/sqlx@latest
go get github.com/go-sql-driver/mysql@latest
```

- [ ] **Step 2: 写失败测试（连接 ping，无 DSN 则跳过）**

`internal/repo/db_test.go`：

```go
package repo

import "testing"

func TestNewDBPing(t *testing.T) {
	dsn := testDSN(t) // 无 TEST_MYSQL_DSN 时 Skip
	db, err := NewDB(dsn)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}
```

- [ ] **Step 3: 运行确认失败**

Run: `go test ./internal/repo/`
Expected: FAIL（`NewDB`、`testDSN` 未定义）

- [ ] **Step 4: 实现**

`internal/repo/db.go`：

```go
// Package repo 是 MySQL DAO 层。业务包只 import 本包，
// 不直接持有 *sqlx.DB，保证未来可替换存储实现。
package repo

import (
	"github.com/jmoiron/sqlx"
	_ "github.com/go-sql-driver/mysql"
)

// NewDB 建立连接池并 ping 验证。连接数按单机个人规模设置。
func NewDB(dsn string) (*sqlx.DB, error) {
	db, err := sqlx.Connect("mysql", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	return db, nil
}
```

`internal/repo/testutil.go`：

```go
package repo

import (
	"os"
	"testing"
)

// testDSN 返回集成测试 DSN；未设置 TEST_MYSQL_DSN 时跳过测试。
// 用法：make test-integration（自动起 docker MySQL + 迁移 + 设置 DSN）。
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN 未设置，跳过集成测试")
	}
	return dsn
}
```

- [ ] **Step 5: 运行测试通过（两种模式）**

```bash
go test ./internal/repo/            # 跳过
make test-integration               # 实际跑通
```

Expected: 分别 SKIP / PASS

- [ ] **Step 6: 提交**

```bash
git add internal/repo/ go.mod go.sum
git commit -m "feat: sqlx 接入与集成测试开关"
```

---

### Task 6: audio_session / pipeline_job DAO

**Files:**
- Create: `internal/repo/session.go`、`internal/repo/job.go`
- Test: `internal/repo/session_test.go`、`internal/repo/job_test.go`

- [ ] **Step 1: 写失败测试（session DAO）**

`internal/repo/session_test.go`：

```go
package repo

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
)

func newTestSession(id ids.ID) *AudioSession {
	return &AudioSession{
		ID: id, UserID: 1, Source: "web_upload", Filename: "a.wav",
		StoragePath: "/tmp/a.wav", Mime: "audio/wav", Status: "uploaded",
	}
}

func TestSessionCreateGet(t *testing.T) {
	db, err := NewDB(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	r := &SessionRepo{DB: db}
	id := ids.New()
	ctx := context.Background()

	if err := r.Create(ctx, newTestSession(id)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := r.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Filename != "a.wav" || got.Status != "uploaded" {
		t.Fatalf("got %+v", got)
	}
}

func TestSessionListAndUpdateStatus(t *testing.T) {
	db, _ := NewDB(testDSN(t))
	r := &SessionRepo{DB: db}
	id := ids.New()
	ctx := context.Background()
	_ = r.Create(ctx, newTestSession(id))

	if err := r.UpdateStatus(ctx, id, "processing"); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	list, err := r.List(ctx, 20, 0)
	if err != nil || len(list) == 0 {
		t.Fatalf("List: %v len=%d", err, len(list))
	}
}
```

- [ ] **Step 2: 写失败测试（job DAO）**

`internal/repo/job_test.go`：

```go
package repo

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
)

func TestJobLifecycle(t *testing.T) {
	db, _ := NewDB(testDSN(t))
	sr := &SessionRepo{DB: db}
	jr := &JobRepo{DB: db}
	ctx := context.Background()

	sid := ids.New()
	if err := sr.Create(ctx, newTestSession(sid)); err != nil {
		t.Fatal(err)
	}

	j := &Job{SessionID: sid, Stage: "asr", Status: "pending"}
	if err := jr.Create(ctx, j); err != nil {
		t.Fatalf("Create: %v", err)
	}

	claimed, ok, err := jr.ClaimNext(ctx)
	if err != nil || !ok {
		t.Fatalf("ClaimNext: %v ok=%v", err, ok)
	}
	if claimed.ID != j.ID || claimed.Status != "running" {
		t.Fatalf("claimed %+v", claimed)
	}

	claimed.Stage = "segment"
	claimed.Status = "pending"
	claimed.Attempt = 0
	if err := jr.Save(ctx, claimed); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, _ := jr.Get(ctx, j.ID)
	if got.Stage != "segment" {
		t.Fatalf("want segment, got %s", got.Stage)
	}
}

// 重启恢复：running 的任务要能被重置回 pending
func TestResetRunning(t *testing.T) {
	db, _ := NewDB(testDSN(t))
	jr := &JobRepo{DB: db}
	ctx := context.Background()
	n, err := jr.ResetRunning(ctx)
	if err != nil {
		t.Fatalf("ResetRunning: %v", err)
	}
	_ = n // 只要不出错即可
}
```

- [ ] **Step 3: 运行确认失败**

Run: `make test-integration`
Expected: FAIL（类型未定义）

- [ ] **Step 4: 实现**

`internal/repo/session.go`：

```go
package repo

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

type AudioSession struct {
	ID          ids.ID    `db:"id" json:"id"`
	UserID      int64     `db:"user_id" json:"user_id"`
	Source      string    `db:"source" json:"source"`
	Filename    string    `db:"filename" json:"filename"`
	StoragePath string    `db:"storage_path" json:"-"`
	DurationMS  int64     `db:"duration_ms" json:"duration_ms"`
	Mime        string    `db:"mime" json:"mime"`
	Status      string    `db:"status" json:"status"`
	JobID       *ids.ID   `db:"job_id" json:"job_id,omitempty"`
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time `db:"updated_at" json:"updated_at"`
}

type SessionRepo struct{ DB *sqlx.DB }

func (r *SessionRepo) Create(ctx context.Context, s *AudioSession) error {
	_, err := r.DB.NamedExecContext(ctx, `
INSERT INTO audio_session (id, user_id, source, filename, storage_path, duration_ms, mime, status)
VALUES (:id, :user_id, :source, :filename, :storage_path, :duration_ms, :mime, :status)`, s)
	return err
}

func (r *SessionRepo) Get(ctx context.Context, id ids.ID) (*AudioSession, error) {
	var s AudioSession
	err := r.DB.GetContext(ctx, &s, `SELECT * FROM audio_session WHERE id = ?`, id.Int64())
	return &s, err
}

func (r *SessionRepo) List(ctx context.Context, limit, offset int) ([]AudioSession, error) {
	var list []AudioSession
	err := r.DB.SelectContext(ctx, &list,
		`SELECT * FROM audio_session ORDER BY id DESC LIMIT ? OFFSET ?`, limit, offset)
	return list, err
}

func (r *SessionRepo) UpdateStatus(ctx context.Context, id ids.ID, status string) error {
	_, err := r.DB.ExecContext(ctx, `UPDATE audio_session SET status = ? WHERE id = ?`, status, id.Int64())
	return err
}

func (r *SessionRepo) SetJobID(ctx context.Context, id ids.ID, jobID ids.ID) error {
	_, err := r.DB.ExecContext(ctx, `UPDATE audio_session SET job_id = ? WHERE id = ?`, jobID.Int64(), id.Int64())
	return err
}
```

`internal/repo/job.go`：

```go
package repo

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// TraceEntry 记录一个 stage 的执行信息，对应 spec 的可观测性要求。
type TraceEntry struct {
	Stage  string `json:"stage"`
	Model  string `json:"model,omitempty"`
	MS     int64  `json:"ms"`
	Error  string `json:"error,omitempty"`
	At     time.Time `json:"at"`
}

type Job struct {
	ID        ids.ID          `db:"id" json:"id"`
	UserID    int64           `db:"user_id" json:"user_id"`
	SessionID ids.ID          `db:"session_id" json:"session_id"`
	Stage     string          `db:"stage" json:"stage"`
	Status    string          `db:"status" json:"status"`
	Attempt   int             `db:"attempt" json:"attempt"`
	LastError *string         `db:"last_error" json:"last_error,omitempty"`
	Trace     json.RawMessage `db:"trace" json:"trace,omitempty"`
	CreatedAt time.Time       `db:"created_at" json:"created_at"`
	UpdatedAt time.Time       `db:"updated_at" json:"updated_at"`
}

type JobRepo struct{ DB *sqlx.DB }

func (r *JobRepo) Create(ctx context.Context, j *Job) error {
	j.ID = ids.New()
	if j.UserID == 0 {
		j.UserID = 1
	}
	_, err := r.DB.NamedExecContext(ctx, `
INSERT INTO pipeline_job (id, user_id, session_id, stage, status)
VALUES (:id, :user_id, :session_id, :stage, :status)`, j)
	return err
}

func (r *JobRepo) Get(ctx context.Context, id ids.ID) (*Job, error) {
	var j Job
	err := r.DB.GetContext(ctx, &j, `SELECT * FROM pipeline_job WHERE id = ?`, id.Int64())
	return &j, err
}

// ClaimNext 原子领取最老的 pending 任务并置为 running。
// 单进程内多 worker 竞争由 UPDATE 行锁保证不重复领取。
func (r *JobRepo) ClaimNext(ctx context.Context) (*Job, bool, error) {
	var id int64
	err := r.DB.GetContext(ctx, &id, `
SELECT id FROM (
  SELECT id FROM pipeline_job WHERE status = 'pending' ORDER BY id LIMIT 1
) t FOR UPDATE`)
	if err != nil {
		if err.Error() == "sql: no rows" {
			return nil, false, nil
		}
		return nil, false, err
	}
	res, err := r.DB.ExecContext(ctx,
		`UPDATE pipeline_job SET status = 'running' WHERE id = ? AND status = 'pending'`, id)
	if err != nil {
		return nil, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, false, nil // 被其他 worker 抢走
	}
	j, err := r.Get(ctx, ids.ID(id))
	return j, true, err
}

func (r *JobRepo) Save(ctx context.Context, j *Job) error {
	trace := "[]"
	if len(j.Trace) > 0 {
		trace = string(j.Trace)
	}
	_, err := r.DB.ExecContext(ctx, `
UPDATE pipeline_job SET stage = ?, status = ?, attempt = ?, last_error = ?, trace = ?
WHERE id = ?`,
		j.Stage, j.Status, j.Attempt, j.LastError, trace, j.ID.Int64())
	return err
}

// ResetRunning 把所有 running 任务重置为 pending（服务重启时调用，任务不丢）。
func (r *JobRepo) ResetRunning(ctx context.Context) (int64, error) {
	res, err := r.DB.ExecContext(ctx, `UPDATE pipeline_job SET status = 'pending' WHERE status = 'running'`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
```

- [ ] **Step 5: 运行测试通过**

Run: `make test-integration`
Expected: repo 包全部 PASS

- [ ] **Step 6: 提交**

```bash
git add internal/repo/
git commit -m "feat: audio_session/pipeline_job DAO 与原子任务领取"
```

---

### Task 7: Ark LLM Provider（OpenAI 兼容）+ Spike 命令

**Files:**
- Create: `internal/provider/llm.go`、`cmd/spike/llm/main.go`
- Test: `internal/provider/llm_test.go`

- [ ] **Step 1: 写失败测试（httptest 假 Ark 服务）**

`internal/provider/llm_test.go`：

```go
package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestArkLLMChat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/chat/completions") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("missing bearer auth")
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "doubao-seed-1.6-flash" {
			t.Errorf("model = %v", body["model"])
		}
		_, _ = w.Write([]byte(`{
			"choices": [{"message": {"content": "{\"ok\":true}"}}],
			"usage": {"total_tokens": 42}
		}`))
	}))
	defer srv.Close()

	p := NewArkLLM(srv.URL, "test-key")
	resp, err := p.Chat(context.Background(), ChatRequest{
		Model: "doubao-seed-1.6-flash",
		System: "你是助手",
		User:   "你好",
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != `{"ok":true}` {
		t.Fatalf("content = %s", resp.Content)
	}
	if resp.TotalTokens != 42 {
		t.Fatalf("tokens = %d", resp.TotalTokens)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/provider/`
Expected: FAIL

- [ ] **Step 3: 实现**

`internal/provider/llm.go`：

```go
// Package provider 抽象全部 AI 能力。业务代码只依赖接口，
// 具体实现（火山 Ark）可整体替换。
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// LLMProvider 是大模型调用接口。
type LLMProvider interface {
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
}

type ChatRequest struct {
	Model       string  // 模型名或 endpoint id（ep-xxx）
	System      string  // system prompt
	User        string  // 用户输入
	Temperature float64 // 0 表示用服务端默认
}

type ChatResponse struct {
	Content     string
	TotalTokens int
}

// ArkLLM 走 Ark 的 OpenAI 兼容 chat/completions 接口。
type ArkLLM struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

func NewArkLLM(baseURL, apiKey string) *ArkLLM {
	return &ArkLLM{baseURL: baseURL, apiKey: apiKey, client: &http.Client{Timeout: 60 * time.Second}}
}

type chatPayload struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature *float64      `json:"temperature,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResp struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (p *ArkLLM) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	msgs := []chatMessage{}
	if req.System != "" {
		msgs = append(msgs, chatMessage{Role: "system", Content: req.System})
	}
	msgs = append(msgs, chatMessage{Role: "user", Content: req.User})

	pl := chatPayload{Model: req.Model, Messages: msgs}
	if req.Temperature > 0 {
		pl.Temperature = &req.Temperature
	}
	body, _ := json.Marshal(pl)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return ChatResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return ChatResponse{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var cr chatResp
	if err := json.Unmarshal(raw, &cr); err != nil {
		return ChatResponse{}, fmt.Errorf("ark llm 响应解析失败 (http %d): %s", resp.StatusCode, truncate(raw))
	}
	if cr.Error != nil {
		return ChatResponse{}, fmt.Errorf("ark llm 错误: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return ChatResponse{}, fmt.Errorf("ark llm 空响应 (http %d): %s", resp.StatusCode, truncate(raw))
	}
	return ChatResponse{Content: strings.TrimSpace(cr.Choices[0].Message.Content), TotalTokens: cr.Usage.TotalTokens}, nil
}

func truncate(b []byte) string {
	if len(b) > 500 {
		return string(b[:500]) + "..."
	}
	return string(b)
}
```

- [ ] **Step 4: 运行测试通过**

Run: `go test ./internal/provider/ -v`
Expected: PASS

- [ ] **Step 5: 写 Spike 命令并真实调用**

`cmd/spike/llm/main.go`：

```go
// spike/llm 用真实 ARK_API_KEY 验证 Ark LLM 连通性（手动运行，不进 CI）。
package main

import (
	"context"
	"fmt"
	"os"

	"zhiwei/internal/config"
	"zhiwei/internal/provider"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Println("config:", err)
		os.Exit(1)
	}
	p := provider.NewArkLLM(cfg.ARKBaseURL, cfg.ARKAPIKey)
	resp, err := p.Chat(context.Background(), provider.ChatRequest{
		Model:  cfg.LLMFastModel,
		System: "你只能回复 JSON。",
		User:   `用 JSON 回答：{"hello":"world"} 的键是什么？`,
	})
	if err != nil {
		fmt.Println("FAIL:", err)
		os.Exit(1)
	}
	fmt.Printf("OK model=%s tokens=%d content=%s\n", cfg.LLMFastModel, resp.TotalTokens, resp.Content)
}
```

Run: `go run ./cmd/spike/llm`
Expected: `OK model=doubao-seed-1.6-flash ...`。若报模型不存在，到火山控制台确认模型 ID 或 endpoint（ep-xxx），用 `ZW_LLM_FAST=ep-xxx` 重试。

- [ ] **Step 6: 提交**

```bash
git add internal/provider/ cmd/spike/
git commit -m "feat: Ark LLM provider 与 spike 命令"
```

---

### Task 8: Ark Embedding Provider + Spike 命令

**Files:**
- Create: `internal/provider/embed.go`、`cmd/spike/embed/main.go`
- Test: `internal/provider/embed_test.go`

- [ ] **Step 1: 写失败测试**

`internal/provider/embed_test.go`：

```go
package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestArkEmbed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2,0.3]},{"embedding":[0.4,0.5,0.6]}]}`))
	}))
	defer srv.Close()

	p := NewArkEmbed(srv.URL, "test-key")
	vecs, err := p.Embed(context.Background(), []string{"你好", "世界"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != 2 || len(vecs[0]) != 3 {
		t.Fatalf("vecs = %v", vecs)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/provider/`
Expected: FAIL（`NewArkEmbed` 未定义）

- [ ] **Step 3: 实现**

`internal/provider/embed.go`：

```go
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type EmbeddingProvider interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// ArkEmbed 走 Ark OpenAI 兼容 /embeddings 接口。
type ArkEmbed struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

func NewArkEmbed(baseURL, apiKey string) *ArkEmbed {
	return &ArkEmbed{baseURL: baseURL, apiKey: apiKey, client: &http.Client{Timeout: 30 * time.Second}}
}

type embedResp struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (p *ArkEmbed) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	// Ark 单次请求有输入条数上限，按 16 条分批
	var all [][]float32
	for i := 0; i < len(texts); i += 16 {
		end := i + 16
		if end > len(texts) {
			end = len(texts)
		}
		batch, err := p.embedBatch(ctx, texts[i:end])
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
	}
	return all, nil
}

func (p *ArkEmbed) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	model := envOrDefault("ZW_EMBED_MODEL", "doubao-embedding-large")
	body, _ := json.Marshal(map[string]any{"model": model, "input": texts})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var er embedResp
	if err := json.Unmarshal(raw, &er); err != nil {
		return nil, fmt.Errorf("ark embed 响应解析失败 (http %d): %s", resp.StatusCode, truncate(raw))
	}
	if er.Error != nil {
		return nil, fmt.Errorf("ark embed 错误: %s", er.Error.Message)
	}
	out := make([][]float32, len(er.Data))
	for i, d := range er.Data {
		out[i] = d.Embedding
	}
	return out, nil
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
```

注意：补上 `os` import。

- [ ] **Step 4: 运行测试通过**

Run: `go test ./internal/provider/ -v`
Expected: PASS

- [ ] **Step 5: Spike 命令与真实调用**

`cmd/spike/embed/main.go`：

```go
// spike/embed 用真实 key 验证 Ark Embedding 连通性（手动运行）。
package main

import (
	"context"
	"fmt"
	"os"

	"zhiwei/internal/config"
	"zhiwei/internal/provider"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Println("config:", err)
		os.Exit(1)
	}
	_ = cfg
	os.Setenv("ZW_EMBED_MODEL", cfg.EmbedModel)
	p := provider.NewArkEmbed(cfg.ARKBaseURL, cfg.ARKAPIKey)
	vecs, err := p.Embed(context.Background(), []string{"今天和朋友讨论了 Rust 学习计划"})
	if err != nil {
		fmt.Println("FAIL:", err)
		os.Exit(1)
	}
	fmt.Printf("OK dim=%d first3=%v\n", len(vecs[0]), vecs[0][:3])
}
```

Run: `go run ./cmd/spike/embed`
Expected: `OK dim=2048 first3=[...]`（维度以实际返回为准）

- [ ] **Step 6: 提交**

```bash
git add internal/provider/ cmd/spike/
git commit -m "feat: Ark embedding provider 与 spike 命令"
```

---

### Task 9: ASR Spike —— Ark ASR 协议验证（Sprint 0 核心）

> 官方文档页是 JS 渲染，无法离线确认报文格式。本 Task 的代码是**按字节跳动大模型流式 ASR 公开协议写的完整草稿**：先跑，若 Ark 实际端点/报文有出入（跑挂或返回错误帧），按错误信息 + 控制台文档（用户登录后可看 https://www.volcengine.com/docs/6561/1354871 ）调整文件顶部标注 `SPIKE-TUNING` 的常量与请求字段，**协议骨架（连接→握手→分片发送→收结果）不会变**。结论必须写入协议笔记。

**Files:**
- Create: `cmd/spike/asr/main.go`、`docs/superpowers/specs/asr-protocol-notes.md`
- Modify: `Makefile`（spike 目标）、`go.mod`（gorilla/websocket）

- [ ] **Step 1: 装依赖**

```bash
go get github.com/gorilla/websocket@latest
```

- [ ] **Step 2: 写 Spike 客户端**

`cmd/spike/asr/main.go`：

```go
// spike/asr 验证 Ark 上的 doubao-seed-asr-2-0 调用协议（手动运行）。
//
// SPIKE-TUNING 项：按官方文档/实测调整
//   - endpoint（默认 wss://ark.cn-beijing.volces.com/api/v3/asr）
//   - fullRequest 里的 audio/request 字段（是否支持 speaker 分离等）
//   - 响应 JSON 的字段路径（text/utterances/speaker）
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/gorilla/websocket"

	"zhiwei/internal/config"
)

// ---- 二进制帧协议（字节大模型 ASR v3，4 字节头 + payload）----
// byte0: protocol(4bit)<<4 | headerSize(4bit)      -> 0x11 (v1, header 4B)
// byte1: msgType(4bit)<<4 | flags(4bit)            -> full=0x11(含seq) / audio=0xB0 / ...
// byte2: serialization(4bit)<<4 | compression(4bit) -> JSON+gzip=0x11 / JSON+none=0x10
// byte3: reserved
const (
	msgFullClient  = 0b0001 // 全量请求（首帧，含配置 JSON）
	msgAudioOnly   = 0b1010 // 纯音频帧
	msgFullServer  = 0b1001 // 服务端全量响应
	msgServerError = 0b1011 // 服务端错误
	flagSeq        = 0b0001 // 序号校验（last packet 时 msgAudioOnly|0b0100 表示结束）
)

func gzipCompress(b []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(b)
	_ = zw.Close()
	return buf.Bytes()
}

func buildFrame(msgType byte, flags byte, payload []byte, useGzip bool) []byte {
	comp := byte(0x0)
	if useGzip {
		comp = 0x1
		payload = gzipCompress(payload)
	}
	hdr := []byte{0x11, msgType<<4 | flags, 0x1<<4 | comp, 0x00}
	return append(hdr, payload...)
}

func parseFrame(data []byte) (msgType byte, payload []byte) {
	msgType = data[1] >> 4
	comp := data[2] & 0x0F
	payload = data[4:]
	if comp == 0x1 {
		zr, err := gzip.NewReader(bytes.NewReader(payload))
		if err == nil {
			raw, _ := io.ReadAll(zr)
			payload = raw
		}
	}
	return msgType, payload
}

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Println("config:", err)
		os.Exit(1)
	}
	audioPath := os.Args[1] // 用法: go run ./cmd/spike/asr testdata/sample.wav
	if audioPath == "" {
		fmt.Println("usage: asr <audio.wav>")
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	endpoint := cfg.ASREndpoint // SPIKE-TUNING: 端点
	dialer := websocket.Dialer{}
	conn, _, err := dialer.DialContext(ctx, endpoint,
		map[string][]string{"Authorization": {"Bearer " + cfg.ARKAPIKey}})
	if err != nil {
		fmt.Println("dial FAIL:", err)
		os.Exit(1)
	}
	defer conn.Close()

	// SPIKE-TUNING: 首帧配置字段（说话人分离字段名以文档为准，可能叫
	// enable_speaker / enable_diarization / show_speaker 等）
	fullReq, _ := json.Marshal(map[string]any{
		"model": cfg.ASRModel,
		"audio": map[string]any{"format": "wav", "rate": 16000, "bits": 16, "channel": 1},
		"request": map[string]any{
			"language": "zh-CN",
			"result_type": "full",
			"enable_punc": true,
			"enable_speaker": true, // 说话人分离
		},
	})
	if err := conn.WriteMessage(websocket.BinaryMessage, buildFrame(msgFullClient, flagSeq, fullReq, true)); err != nil {
		fmt.Println("full request FAIL:", err)
		os.Exit(1)
	}

	// 发送音频：16KB 一片，最后一片带结束标志
	raw, err := os.ReadFile(audioPath)
	if err != nil {
		fmt.Println("read audio:", err)
		os.Exit(1)
	}
	const chunk = 16 * 1024
	go func() {
		for off := 0; off < len(raw); off += chunk {
			end := off + chunk
			if end > len(raw) {
				end = len(raw)
			}
			last := end >= len(raw)
			flags := byte(0)
			if last {
				flags = 0b0100 // last packet
			}
			frame := buildFrame(msgAudioOnly, flags, raw[off:end], false)
			_ = conn.WriteMessage(websocket.BinaryMessage, frame)
			time.Sleep(40 * time.Millisecond) // 模拟实时率 < 1
		}
	}()

	// 收结果：打印每一帧原始 JSON，人工确认字段结构
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			fmt.Println("\nread end:", err)
			return
		}
		mt, payload := parseFrame(data)
		if mt == msgServerError {
			fmt.Println("SERVER ERROR:", string(payload))
			os.Exit(1)
		}
		if mt == msgFullServer {
			fmt.Printf("SERVER MSG: %s\n", payload)
		}
	}
}
```

- [ ] **Step 3: 准备测试音频**

```bash
mkdir -p testdata
# 生成 5 秒 16k mono 正弦波（能验证链路；识别不出内容属正常）
ffmpeg -f lavfi -i "sine=frequency=440:duration=5" -ar 16000 -ac 1 -sample_fmt s16 testdata/sample.wav
```

有条件的话换成人声文件（比如手机录一句「明天记得给 Tom 发邮件」）效果最好。

- [ ] **Step 4: 运行并迭代**

```bash
go run ./cmd/spike/asr testdata/sample.wav
```

迭代流程：
1. 若 `dial FAIL` → 端点不对。查文档确认 Ark ASR 的 WebSocket 地址，改 `ZW_ASR_ENDPOINT` 环境变量重试（不改代码即可测多个候选端点）。
2. 若连上但报 SERVER ERROR → 对照错误信息调整首帧 JSON 字段（SPIKE-TUNING 注释处）。
3. 若收到的消息不是 4 字节头二进制帧（`parseFrame` 解出乱码）→ Ark 可能用纯 JSON 文本消息协议：把 ReadMessage 分支改为直接打印 `string(data)`，按实际结构重写解析。
4. 成功标准：收到含识别文本（及说话人字段）的 SERVER MSG。

- [ ] **Step 5: 把结论写进协议笔记**

`docs/superpowers/specs/asr-protocol-notes.md`（按实际结果填写，模板如下）：

```markdown
# Ark ASR (doubao-seed-asr-2-0) 协议实测笔记

- 实测端点：
- 鉴权方式：
- 首帧配置（最终字段）：
- 音频帧格式与结束标志：
- 响应报文结构与字段路径（识别文本 / 说话人标签 / 时间戳 / 置信度）：
- 说话人分离输出示例：
- 速率限制与注意事项：
```

- [ ] **Step 6: Makefile 加 spike 目标并提交**

Makefile 追加：

```makefile
spike-llm:
	go run ./cmd/spike/llm
spike-embed:
	go run ./cmd/spike/embed
spike-asr:
	go run ./cmd/spike/asr testdata/sample.wav
```

```bash
git add cmd/spike/asr/ docs/superpowers/specs/asr-protocol-notes.md Makefile go.mod go.sum
git commit -m "spike: Ark ASR 协议验证与结论笔记"
```

---

### Task 10: ASR Provider 封装（基于 Spike 结论）

**Files:**
- Create: `internal/provider/asr.go`
- Test: `internal/provider/asr_test.go`

- [ ] **Step 1: 定义接口与输出结构（写失败测试：纯解析逻辑）**

`internal/provider/asr_test.go`（`parseUtterances` 的输入 JSON 结构以 Task 9 笔记为准，以下按预期结构写；若笔记不同，测试样例同步替换）：

```go
package provider

import (
	"testing"

	"zhiwei/internal/ids"
)

func TestParseUtterances(t *testing.T) {
	raw := []byte(`{
	  "result": "{\"text\":\"你好今天开会\",\"utterances\":[{\"text\":\"你好\",\"start_time\":0,\"end_time\":500,\"speaker\":1,\"confidence\":0.95},{\"text\":\"今天开会\",\"start_time\":600,\"end_time\":1500,\"speaker\":2,\"confidence\":0.9}]}"
	}`)
	pieces, err := parseUtterances(raw)
	if err != nil {
		t.Fatalf("parseUtterances: %v", err)
	}
	if len(pieces) != 2 {
		t.Fatalf("len = %d", len(pieces))
	}
	if pieces[0].Text != "你好" || pieces[0].SpeakerLabel != "1" {
		t.Fatalf("pieces[0] = %+v", pieces[0])
	}
	if pieces[1].StartMS != 600 {
		t.Fatalf("pieces[1].StartMS = %d", pieces[1].StartMS)
	}
	_ = ids.New // 保持 import
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/provider/`
Expected: FAIL（`parseUtterances`、`TranscriptPiece` 未定义）

- [ ] **Step 3: 实现**

`internal/provider/asr.go`：

```go
package provider

import (
	"context"
	"encoding/json"
	"fmt"
)

// TranscriptPiece 是 ASR 输出的最小转写单元：一段话 + 说话人标签。
type TranscriptPiece struct {
	SpeakerLabel string  // ASR 原始标签（"1"/"2"/...），Person 实体映射下一期做
	Text         string
	StartMS      int64
	EndMS        int64
	Confidence   float64
}

type ASRProvider interface {
	// Transcribe 输入音频文件路径（调用方保证已转成 wav 16k mono），返回转写片段。
	Transcribe(ctx context.Context, audioPath string) ([]TranscriptPiece, error)
}

// ---- 以下 Ark 实现的帧收发代码 = Task 9 spike 验证通过的代码搬进来 ----
// 搬运时：cmd/spike/asr/main.go 里的 buildFrame/parseFrame/gzip 辅助与
// 主流程（dial → 首帧 → 分片发 → 收帧）移到本文件，
// 端点/模型从构造参数传入。此处不再重复贴码，以 spike 最终代码为准。

// NewArkASR 构造 Ark ASR 客户端。
func NewArkASR(endpoint, apiKey, model string) *ArkASR {
	return &ArkASR{endpoint: endpoint, apiKey: apiKey, model: model}
}

type ArkASR struct {
	endpoint string
	apiKey   string
	model    string
}

// Transcribe 实现 ASRProvider（内部执行 spike 验证过的收发流程）。
func (p *ArkASR) Transcribe(ctx context.Context, audioPath string) ([]TranscriptPiece, error) {
	return p.transcribe(ctx, audioPath) // transcribe = spike 主流程搬入（见上注释）
}

// parseUtterances 从最终响应 JSON 提取转写片段（纯函数，可单测）。
// 字段路径按 asr-protocol-notes.md 实测结论维护。
func parseUtterances(finalPayload []byte) ([]TranscriptPiece, error) {
	var outer struct {
		Result string `json:"result"` // 字节系协议里内层是字符串化 JSON
	}
	if err := json.Unmarshal(finalPayload, &outer); err != nil {
		return nil, fmt.Errorf("asr 响应解析失败: %w", err)
	}
	var inner struct {
		Utterances []struct {
			Text      string  `json:"text"`
			StartTime int64   `json:"start_time"` // 毫秒
			EndTime   int64   `json:"end_time"`
			Speaker   any     `json:"speaker"`   // 可能是 number 或 string
			Confidence float64 `json:"confidence"`
		} `json:"utterances"`
	}
	if err := json.Unmarshal([]byte(outer.Result), &inner); err != nil {
		return nil, fmt.Errorf("asr utterances 解析失败: %w", err)
	}
	out := make([]TranscriptPiece, 0, len(inner.Utterances))
	for _, u := range inner.Utterances {
		out = append(out, TranscriptPiece{
			SpeakerLabel: fmt.Sprintf("%v", u.Speaker),
			Text:         u.Text,
			StartMS:      u.StartTime,
			EndMS:        u.EndTime,
			Confidence:   u.Confidence,
		})
	}
	return out, nil
}
```

- [ ] **Step 4: 运行测试通过 + 真实回归**

```bash
go test ./internal/provider/ -v
go run ./cmd/spike/asr testdata/sample.wav   # 确认搬运后仍通
```

Expected: PASS / 正常收到识别结果

- [ ] **Step 5: 提交**

```bash
git add internal/provider/
git commit -m "feat: ASR provider 接口与 Ark 实现（封装 spike 结论）"
```

---

### Task 11: Worker 池与 stage 状态机

**Files:**
- Create: `internal/pipeline/state.go`、`internal/pipeline/pool.go`
- Test: `internal/pipeline/state_test.go`、`internal/pipeline/pool_test.go`

- [ ] **Step 1: 写失败测试（纯状态逻辑）**

`internal/pipeline/state_test.go`：

```go
package pipeline

import (
	"errors"
	"testing"
)

func TestNextStage(t *testing.T) {
	// Sprint 1 流水线：asr -> segment -> done（extract/quality/commit 在 Sprint 2 注入）
	flow := Flow{Stages: []string{"asr", "segment"}}
	if got := flow.Next("asr"); got != "segment" {
		t.Errorf("Next(asr) = %s", got)
	}
	if got := flow.Next("segment"); got != StageDone {
		t.Errorf("Next(segment) = %s", got)
	}
}

func TestApplySuccess(t *testing.T) {
	flow := Flow{Stages: []string{"asr", "segment"}}
	j := &JobState{Stage: "asr", Status: "running", Attempt: 2}
	err := flow.Apply(j, nil)
	if err != nil {
		t.Fatal(err)
	}
	if j.Stage != "segment" || j.Status != "pending" || j.Attempt != 0 {
		t.Fatalf("job = %+v", j)
	}
}

func TestApplyFailureRetryThenFail(t *testing.T) {
	flow := Flow{Stages: []string{"asr", "segment"}, MaxAttempt: 3}
	j := &JobState{Stage: "asr", Status: "running"}
	// 失败 1、2 次：回 pending，attempt 累加
	for wantAttempt := 1; wantAttempt <= 2; wantAttempt++ {
		if err := flow.Apply(j, errors.New("boom")); err != nil {
			t.Fatal(err)
		}
		if j.Status != "pending" || j.Attempt != wantAttempt {
			t.Fatalf("attempt %d: job = %+v", wantAttempt, j)
		}
	}
	// 第 3 次失败：进 failed
	if err := flow.Apply(j, errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	if j.Status != "failed" || j.Stage != "asr" {
		t.Fatalf("final job = %+v", j)
	}
}

func TestApplyDone(t *testing.T) {
	flow := Flow{Stages: []string{"asr", "segment"}}
	j := &JobState{Stage: "segment", Status: "running"}
	if err := flow.Apply(j, nil); err != nil {
		t.Fatal(err)
	}
	if j.Status != "done" {
		t.Fatalf("job = %+v", j)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/pipeline/`
Expected: FAIL

- [ ] **Step 3: 实现 state.go**

`internal/pipeline/state.go`：

```go
// state.go 是 pipeline 的纯逻辑：stage 推进与重试决策。
// 不碰 DB / 网络，保证可以完全单元测试。
package pipeline

// StageDone 表示整条流水线完成。
const StageDone = "done"

// JobState 是 pool 与 DAO 之间传递的任务快照（repo.Job 的子集，
// 拆出来是为了让状态机不依赖 sqlx）。
type JobState struct {
	ID      int64
	Stage   string
	Status  string
	Attempt int
}

// Flow 描述一条 stage 流水线及其重试策略。
type Flow struct {
	Stages     []string // 按执行顺序
	MaxAttempt int      // 每 stage 最大尝试次数（含首次）
}

// Next 返回下一 stage；已是最后一个则返回 StageDone。
func (f Flow) Next(stage string) string {
	for i, s := range f.Stages {
		if s == stage {
			if i+1 < len(f.Stages) {
				return f.Stages[i+1]
			}
			return StageDone
		}
	}
	return StageDone
}

// Apply 把一次执行结果应用到任务状态上（原地修改）。
// err == nil：推进到下一 stage（或 done），attempt 清零。
// err != nil：attempt+1；未超上限回 pending 重试，超了进 failed。
func (f Flow) Apply(j *JobState, err error) error {
	max := f.MaxAttempt
	if max <= 0 {
		max = 3
	}
	if err == nil {
		j.Stage = f.Next(j.Stage)
		j.Attempt = 0
		if j.Stage == StageDone {
			j.Status = "done"
		} else {
			j.Status = "pending"
		}
		return nil
	}
	j.Attempt++
	if j.Attempt >= max {
		j.Status = "failed"
	} else {
		j.Status = "pending"
	}
	return nil
}
```

- [ ] **Step 4: 运行测试通过**

Run: `go test ./internal/pipeline/ -v`
Expected: PASS（4 个用例）

- [ ] **Step 5: 实现 pool.go（含集成冒烟测试）**

`internal/pipeline/pool.go`：

```go
package pipeline

import (
	"context"
	"log"
	"time"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// Handler 执行一个 stage。sessionID 是流水线的处理对象。
// 返回 nil 即成功，状态机推进到下一 stage。
type Handler func(ctx context.Context, sessionID ids.ID) error

// Pool 是进程内 worker 池：轮询领取 pending 任务并执行。
type Pool struct {
	jobs        *repo.JobRepo
	flow        Flow
	handlers    map[string]Handler
	concurrency int
	onDone      func(ctx context.Context, sessionID ids.ID)
}

func NewPool(jobs *repo.JobRepo, flow Flow, handlers map[string]Handler) *Pool {
	return &Pool{jobs: jobs, flow: flow, handlers: handlers, concurrency: 2}
}

// OnDone 注册流水线完成回调（如把 session 置为 completed）。
func (p *Pool) OnDone(fn func(ctx context.Context, sessionID ids.ID)) { p.onDone = fn }

// Start 阻塞式启动：先恢复遗留 running 任务，再启动 worker 循环。
func (p *Pool) Start(ctx context.Context) {
	if n, err := p.jobs.ResetRunning(ctx); err != nil {
		log.Printf("[pool] 恢复 running 任务失败: %v", err)
	} else if n > 0 {
		log.Printf("[pool] 恢复 %d 个中断任务", n)
	}
	for i := 0; i < p.concurrency; i++ {
		go p.loop(ctx)
	}
}

func (p *Pool) loop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.claimAndRun(ctx)
		}
	}
}

func (p *Pool) claimAndRun(ctx context.Context) {
	j, ok, err := p.jobs.ClaimNext(ctx)
	if err != nil {
		log.Printf("[pool] 领取任务失败: %v", err)
		return
	}
	if !ok {
		return
	}
	h, exists := p.handlers[j.Stage]
	st := JobState{ID: j.ID.Int64(), Stage: j.Stage, Status: j.Status, Attempt: j.Attempt}
	var runErr error
	if !exists {
		runErr = errNoHandler(j.Stage)
	} else {
		begin := time.Now()
		runErr = safeRun(ctx, h, j.SessionID)
		log.Printf("[pool] job=%d stage=%s took=%s err=%v", j.ID, j.Stage, time.Since(begin), runErr)
	}
	_ = p.flow.Apply(&st, runErr)
	persist(ctx, p.jobs, j, st, runErr)
	if st.Status == "done" && p.onDone != nil {
		p.onDone(ctx, j.SessionID)
	}
}

func persist(ctx context.Context, r *repo.JobRepo, j *repo.Job, st JobState, runErr error) {
	j.Stage, j.Status, j.Attempt = st.Stage, st.Status, st.Attempt
	if runErr != nil {
		msg := runErr.Error()
		j.LastError = &msg
	}
	if err := r.Save(ctx, j); err != nil {
		log.Printf("[pool] 保存任务状态失败 job=%d: %v", j.ID, err)
	}
}

func safeRun(ctx context.Context, h Handler, sid ids.ID) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panic: %v", r)
		}
	}()
	return h(ctx, sid)
}
```

补一个 `errors.go`（同包小文件）：

```go
package pipeline

import "fmt"

func errNoHandler(stage string) error { return fmt.Errorf("stage %s 未注册 handler", stage) }
```

注意：pool.go 需要 import `fmt`。

`internal/pipeline/pool_test.go`（集成冒烟：真 DB + 假 handler）：

```go
package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// 冒烟：pending 任务被领取执行，成功后推进到 done
func TestPoolRunsJobToDone(t *testing.T) {
	db, _ := repo.NewDB(repo.TestDSN(t))
	jobs := &repo.JobRepo{DB: db}
	sessions := &repo.SessionRepo{DB: db}
	ctx := context.Background()

	sid := ids.New()
	_ = sessions.Create(ctx, &repo.AudioSession{
		ID: sid, Source: "web_upload", Filename: "x.wav",
		StoragePath: "/tmp/x.wav", Status: "uploaded",
	})
	_ = jobs.Create(ctx, &repo.Job{SessionID: sid, Stage: "asr", Status: "pending"})

	done := make(chan ids.ID, 1)
	handlers := map[string]Handler{
		"asr":     func(ctx context.Context, s ids.ID) error { return nil },
		"segment": func(ctx context.Context, s ids.ID) error { return nil },
	}
	p := NewPool(jobs, Flow{Stages: []string{"asr", "segment"}}, handlers)
	p.OnDone(func(_ context.Context, s ids.ID) { done <- s })

	go p.Start(ctx)
	defer func() { /* 测试结束由 ctx 取消兜底 */ }()
	select {
	case got := <-done:
		if got != sid {
			t.Fatalf("done session = %d", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("10s 内未跑到 done")
	}
	j, _ := jobs.Get(ctx, sidToJobID(t, jobs, sid))
	if j.Status != "done" {
		t.Fatalf("job status = %s", j.Status)
	}
}

func sidToJobID(t *testing.T, jobs *repo.JobRepo, sid ids.ID) ids.ID {
	t.Helper()
	var j repo.Job
	if err := jobs.DB.Get(&j, `SELECT * FROM pipeline_job WHERE session_id = ? ORDER BY id DESC LIMIT 1`, sid.Int64()); err != nil {
		t.Fatal(err)
	}
	return j.ID
}

var _ = errors.New // 占位避免未用 import（如不用可删）
```

同时把 `repo/testutil.go` 的 `testDSN` 改为导出 `TestDSN`（供 pipeline 包使用）：

```go
// TestDSN 返回集成测试 DSN；未设置时跳过调用方测试。
func TestDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN 未设置，跳过集成测试")
	}
	return dsn
}
```

- [ ] **Step 6: 运行全部测试通过**

Run: `make test-integration`
Expected: pipeline + repo 全部 PASS

- [ ] **Step 7: 提交**

```bash
git add internal/pipeline/ internal/repo/testutil.go
git commit -m "feat: pipeline worker 池与可单测的 stage 状态机"
```

---

### Task 12: Stage 实现（ffmpeg 转码 + ASR + transcript 落库 + segment 汇总）

**Files:**
- Create: `internal/repo/transcript.go`、`internal/pipeline/stage_asr.go`
- Test: `internal/pipeline/stage_asr_test.go`

- [ ] **Step 1: transcript DAO（无独立测试，由 stage 测试覆盖）**

`internal/repo/transcript.go`：

```go
package repo

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

type Transcript struct {
	ID        ids.ID   `db:"id" json:"id"`
	UserID    int64    `db:"user_id" json:"user_id"`
	SessionID ids.ID   `db:"session_id" json:"session_id"`
	Language  string   `db:"language" json:"language"`
	FullText  *string  `db:"full_text" json:"full_text"`
	Confidence *float64 `db:"confidence" json:"confidence"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

type TranscriptSegment struct {
	ID           ids.ID   `db:"id" json:"id"`
	TranscriptID ids.ID   `db:"transcript_id" json:"transcript_id"`
	SequenceNo   int      `db:"sequence_no" json:"sequence_no"`
	SpeakerLabel string   `db:"speaker_label" json:"speaker_label"`
	Text         string   `db:"text" json:"text"`
	StartMS      int64    `db:"start_ms" json:"start_ms"`
	EndMS        int64    `db:"end_ms" json:"end_ms"`
	Confidence   *float64 `db:"confidence" json:"confidence"`
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
}

type TranscriptRepo struct{ DB *sqlx.DB }

func (r *TranscriptRepo) Create(ctx context.Context, t *Transcript) error {
	t.ID = ids.New()
	if t.UserID == 0 {
		t.UserID = 1
	}
	_, err := r.DB.NamedExecContext(ctx, `
INSERT INTO transcript (id, user_id, session_id, language)
VALUES (:id, :user_id, :session_id, :language)`, t)
	return err
}

func (r *TranscriptRepo) InsertSegments(ctx context.Context, segs []TranscriptSegment) error {
	if len(segs) == 0 {
		return nil
	}
	for i := range segs {
		segs[i].ID = ids.New()
	}
	_, err := r.DB.NamedExecContext(ctx, `
INSERT INTO transcript_segment (id, transcript_id, sequence_no, speaker_label, text, start_ms, end_ms, confidence)
VALUES (:id, :transcript_id, :sequence_no, :speaker_label, :text, :start_ms, :end_ms, :confidence)`, segs)
	return err
}

func (r *TranscriptRepo) GetBySession(ctx context.Context, sessionID ids.ID) (*Transcript, error) {
	var t Transcript
	err := r.DB.GetContext(ctx, &t,
		`SELECT * FROM transcript WHERE session_id = ? ORDER BY id DESC LIMIT 1`, sessionID.Int64())
	return &t, err
}

func (r *TranscriptRepo) ListSegments(ctx context.Context, transcriptID ids.ID) ([]TranscriptSegment, error) {
	var list []TranscriptSegment
	err := r.DB.SelectContext(ctx, &list,
		`SELECT * FROM transcript_segment WHERE transcript_id = ? ORDER BY sequence_no`, transcriptID.Int64())
	return list, err
}

func (r *TranscriptRepo) SetFullText(ctx context.Context, id ids.ID, full string, conf float64) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE transcript SET full_text = ?, confidence = ? WHERE id = ?`, full, conf, id.Int64())
	return err
}
```

- [ ] **Step 2: 写失败测试（假 ASR provider，走完 asr+segment 两个 stage）**

`internal/pipeline/stage_asr_test.go`：

```go
package pipeline

import (
	"context"
	"os"
	"os/exec"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
)

type fakeASR struct{}

func (fakeASR) Transcribe(_ context.Context, _ string) ([]provider.TranscriptPiece, error) {
	return []provider.TranscriptPiece{
		{SpeakerLabel: "1", Text: "明天记得给 Tom 发邮件", StartMS: 0, EndMS: 2000, Confidence: 0.95},
		{SpeakerLabel: "1", Text: "还有确认会议时间", StartMS: 2100, EndMS: 3600, Confidence: 0.92},
		{SpeakerLabel: "2", Text: "好的", StartMS: 3800, EndMS: 4200, Confidence: 0.9},
	}, nil
}

// 无 ffmpeg 环境跳过（stage 内部会调用 ffmpeg 转码）
func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 未安装")
	}
}

func TestStagesASRAndSegment(t *testing.T) {
	requireFFmpeg(t)
	db, _ := repo.NewDB(repo.TestDSN(t))
	ctx := context.Background()

	sessions := &repo.SessionRepo{DB: db}
	jobs := &repo.JobRepo{DB: db}
	transcripts := &repo.TranscriptRepo{DB: db}

	// 用 sine wav 当输入（stage 只做容器转码，不校验内容）
	sid := ids.New()
	_ = sessions.Create(ctx, &repo.AudioSession{
		ID: sid, Source: "web_upload", Filename: "s.wav",
		StoragePath: "testdata/sample.wav", Status: "processing",
	})
	_ = jobs.Create(ctx, &repo.Job{SessionID: sid, Stage: "asr", Status: "pending"})

	handlers := BuildStages(StageDeps{
		Sessions: sessions, Transcripts: transcripts, ASR: fakeASR{}, DataDir: "data",
	})
	pool := NewPool(jobs, Flow{Stages: []string{"asr", "segment"}}, handlers)

	runDone := make(chan ids.ID, 1)
	pool.OnDone(func(_ context.Context, s ids.ID) { runDone <- s })
	go pool.Start(ctx)
	defer os.RemoveAll("data") // 清理转码产物

	select {
	case <-runDone:
	case <-ctx.Done():
		t.Fatal("超时")
	}

	// 断言：transcript + segments 已落库，full_text 已汇总
	tr, err := transcripts.GetBySession(ctx, sid)
	if err != nil {
		t.Fatalf("GetBySession: %v", err)
	}
	if tr.FullText == nil || *tr.FullText == "" {
		t.Fatal("full_text 为空")
	}
	segs, _ := transcripts.ListSegments(ctx, tr.ID)
	if len(segs) != 3 {
		t.Fatalf("segments = %d", len(segs))
	}
}
```

- [ ] **Step 3: 运行确认失败**

Run: `make test-integration`
Expected: FAIL（`BuildStages`、`StageDeps` 未定义）

- [ ] **Step 4: 实现**

`internal/pipeline/stage_asr.go`：

```go
// stage_asr 实现 asr（转码+识别+落库）与 segment（聚合+全文）两个 stage。
package pipeline

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"zhiwei/internal/ids"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
)

// StageDeps 是 stage 的依赖集合（全部接口化便于测试注入）。
type StageDeps struct {
	Sessions    *repo.SessionRepo
	Transcripts *repo.TranscriptRepo
	ASR         provider.ASRProvider
	DataDir     string // 转码输出目录
}

// BuildStages 返回 stage 名 -> handler 的映射，供 Pool 装配。
func BuildStages(d StageDeps) map[string]Handler {
	return map[string]Handler{
		"asr":     stageASR(d),
		"segment": stageSegment(d),
	}
}

// stageASR：ffmpeg 统一转 wav16k → ASR → transcript + segments 落库。
func stageASR(d StageDeps) Handler {
	return func(ctx context.Context, sessionID ids.ID) error {
		s, err := d.Sessions.Get(ctx, sessionID)
		if err != nil {
			return fmt.Errorf("读取 session: %w", err)
		}
		wavPath, err := transcodeToWAV(d.DataDir, sessionID, s.StoragePath)
		if err != nil {
			return fmt.Errorf("转码: %w", err)
		}
		pieces, err := d.ASR.Transcribe(ctx, wavPath)
		if err != nil {
			return fmt.Errorf("asr: %w", err)
		}
		tr := &repo.Transcript{SessionID: sessionID, Language: "zh-CN"}
		if err := d.Transcripts.Create(ctx, tr); err != nil {
			return fmt.Errorf("写 transcript: %w", err)
		}
		segs := make([]repo.TranscriptSegment, len(pieces))
		for i, p := range pieces {
			segs[i] = repo.TranscriptSegment{
				TranscriptID: tr.ID, SequenceNo: i + 1,
				SpeakerLabel: p.SpeakerLabel, Text: p.Text,
				StartMS: p.StartMS, EndMS: p.EndMS, Confidence: &p.Confidence,
			}
		}
		if err := d.Transcripts.InsertSegments(ctx, segs); err != nil {
			return fmt.Errorf("写 segments: %w", err)
		}
		return nil
	}
}

// stageSegment：把 segments 汇总成全文（Sprint 2 的 extract 将以
// 「连续同说话人聚合块」为输入；本 stage 先做全文字段并完成流水线）。
func stageSegment(d StageDeps) Handler {
	return func(ctx context.Context, sessionID ids.ID) error {
		tr, err := d.Transcripts.GetBySession(ctx, sessionID)
		if err != nil {
			return fmt.Errorf("读取 transcript: %w", err)
		}
		segs, err := d.Transcripts.ListSegments(ctx, tr.ID)
		if err != nil {
			return fmt.Errorf("读取 segments: %w", err)
		}
		var sb strings.Builder
		var sumConf, n float64
		for _, s := range segs {
			if s.Text == "" {
				continue
			}
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			fmt.Fprintf(&sb, "[%s] %s", speakerName(s.SpeakerLabel), s.Text)
			if s.Confidence != nil {
				sumConf += *s.Confidence
				n++
			}
		}
		conf := 0.0
		if n > 0 {
			conf = sumConf / n
		}
		return d.Transcripts.SetFullText(ctx, tr.ID, sb.String(), conf)
	}
}

// speakerLabel "1" -> "说话人 1"；空标签 -> "未知说话人"。
func speakerName(label string) string {
	if label == "" {
		return "未知说话人"
	}
	return "说话人 " + label
}

// transcodeToWAV 任意输入转 16k mono s16 wav，输出到 data/transcoded/。
func transcodeToWAV(dataDir string, sessionID ids.ID, src string) (string, error) {
	if err := os.MkdirAll(filepath.Join(dataDir, "transcoded"), 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(dataDir, "transcoded", sessionID.String()+".wav")
	if _, err := os.Stat(dst); err == nil {
		return dst, nil // 幂等：转码产物已存在直接复用
	}
	cmd := exec.Command("ffmpeg", "-y", "-i", src,
		"-ar", "16000", "-ac", "1", "-sample_fmt", "s16", dst)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("ffmpeg 输出: %s", out)
		return "", err
	}
	return dst, nil
}
```

注意 stage 测试里 `select` 分支去掉 `ctx.Done()`，改为超时控制：

```go
	select {
	case <-runDone:
	case <-time.After(30 * time.Second):
		t.Fatal("30s 未跑完")
	}
```

（`time` import 补上；`context.Background()` 不会 Done。）

- [ ] **Step 5: 运行测试通过**

Run: `make test-integration`
Expected: pipeline 全部 PASS（fake ASR + 真转码 + 真落库）

- [ ] **Step 6: 提交**

```bash
git add internal/pipeline/ internal/repo/transcript.go
git commit -m "feat: asr/segment stage 实现（转码+识别+落库+全文汇总）"
```

---

### Task 13: 上传 API

**Files:**
- Create: `internal/api/audio.go`
- Modify: `internal/api/router.go`（挂载路由）、`cmd/zhiwei-server/main.go`（完整装配）
- Test: `internal/api/audio_test.go`

- [ ] **Step 1: 写失败测试**

`internal/api/audio_test.go`：

```go
package api

import (
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

func setupAPI(t *testing.T) http.Handler {
	t.Helper()
	db, err := repo.NewDB(repo.TestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := ids.Init(1); err != nil {
		t.Fatal(err)
	}
	sessions := &repo.SessionRepo{DB: db}
	jobs := &repo.JobRepo{DB: db}
	r := chi.NewRouter()
	RegisterAudio(r, sessions, jobs, dir)
	return r
}

func TestUploadAudio(t *testing.T) {
	handler := setupAPI(t)

	// 构造 multipart 请求
	var body strings.Builder
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "test.webm")
	fw.Write([]byte("fake-audio-bytes"))
	mw.WriteField("source", "web_record")
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/audio", strings.NewReader(body.String()))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 got %d: %s", rec.Code, rec.Body.String())
	}
	resp := rec.Body.String()
	if !strings.Contains(resp, `"session_id"`) || !strings.Contains(resp, `"job_id"`) {
		t.Fatalf("resp = %s", resp)
	}

	// 落盘文件存在（目录里出现转存文件）
	entries, _ := os.ReadDir(filepath.Join(filepath.Dir(rec.Result().Request.URL.Path), "data"))
	_ = entries
}
```

（最后一个断言块无意义可删；有效断言是状态码与响应字段。）

- [ ] **Step 2: 运行确认失败**

Run: `make test-integration`
Expected: FAIL（`RegisterAudio` 未定义）

- [ ] **Step 3: 实现**

`internal/api/audio.go`：

```go
package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// AudioHandler 处理音频上传。
type AudioHandler struct {
	Sessions *repo.SessionRepo
	Jobs     *repo.JobRepo
	DataDir  string
}

// RegisterAudio 挂载音频相关路由。
func RegisterAudio(r chi.Router, sessions *repo.SessionRepo, jobs *repo.JobRepo, dataDir string) {
	h := &AudioHandler{Sessions: sessions, Jobs: jobs, DataDir: dataDir}
	r.Post("/api/audio", h.Upload)
}

var allowedExt = map[string]string{
	".wav": "audio/wav", ".mp3": "audio/mpeg", ".m4a": "audio/mp4",
	".webm": "audio/webm", ".ogg": "audio/ogg", ".flac": "audio/flac",
}

const maxUploadBytes = 200 << 20 // 200MB

func (h *AudioHandler) Upload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "解析上传失败: "+err.Error(), http.StatusBadRequest)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "缺少 file 字段", http.StatusBadRequest)
		return
	}
	defer file.Close()

	ext := strings.ToLower(filepath.Ext(header.Filename))
	mime, ok := allowedExt[ext]
	if !ok {
		http.Error(w, fmt.Sprintf("不支持的音频格式: %s", ext), http.StatusUnsupportedMediaType)
		return
	}
	source := r.FormValue("source")
	if source != "web_record" {
		source = "web_upload"
	}

	if err := os.MkdirAll(filepath.Join(h.DataDir, "uploads"), 0o755); err != nil {
		http.Error(w, "存储目录创建失败", http.StatusInternalServerError)
		return
	}
	sid := ids.New()
	dst := filepath.Join(h.DataDir, "uploads", sid.String()+ext)
	out, err := os.Create(dst)
	if err != nil {
		http.Error(w, "文件创建失败", http.StatusInternalServerError)
		return
	}
	size, err := io.Copy(out, file)
	out.Close()
	if err != nil {
		os.Remove(dst)
		http.Error(w, "文件写入失败", http.StatusInternalServerError)
		return
	}

	s := &repo.AudioSession{
		ID: sid, Source: source, Filename: header.Filename,
		StoragePath: dst, DurationMS: 0, Mime: mime, Status: "processing",
	}
	if err := h.Sessions.Create(r.Context(), s); err != nil {
		http.Error(w, "session 入库失败", http.StatusInternalServerError)
		return
	}
	j := &repo.Job{SessionID: sid, Stage: "asr", Status: "pending"}
	if err := h.Jobs.Create(r.Context(), j); err != nil {
		http.Error(w, "job 入库失败", http.StatusInternalServerError)
		return
	}
	_ = h.Sessions.SetJobID(r.Context(), sid, j.ID)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"session_id": sid,
		"job_id":     j.ID,
		"size":       size,
	})
}
```

`cmd/zhiwei-server/main.go` 重写为完整装配：

```go
// zhiwei-server 是知微云端 MVP 的唯一入口：HTTP API + 异步 pipeline worker。
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"zhiwei/internal/api"
	"zhiwei/internal/config"
	"zhiwei/internal/ids"
	"zhiwei/internal/pipeline"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	if err := ids.Init(1); err != nil {
		log.Fatal(err)
	}
	db, err := repo.NewDB(cfg.MySQLDSN)
	if err != nil {
		log.Fatal(err)
	}

	sessions := &repo.SessionRepo{DB: db}
	jobs := &repo.JobRepo{DB: db}
	transcripts := &repo.TranscriptRepo{DB: db}

	// pipeline 装配
	asr := provider.NewArkASR(cfg.ASREndpoint, cfg.ARKAPIKey, cfg.ASRModel)
	stages := pipeline.BuildStages(pipeline.StageDeps{
		Sessions: sessions, Transcripts: transcripts, ASR: asr, DataDir: cfg.DataDir,
	})
	flow := pipeline.Flow{Stages: []string{"asr", "segment"}}
	pool := pipeline.NewPool(jobs, flow, stages)
	pool.OnDone(func(ctx context.Context, sid ids.ID) {
		_ = sessions.UpdateStatus(ctx, sid, "completed")
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	pool.Start(ctx)

	r := api.NewRouter()
	api.RegisterAudio(r, sessions, jobs, cfg.DataDir)

	srv := &http.Server{Addr: ":" + cfg.Port, Handler: r}
	go func() {
		log.Println("zhiwei-server listening on :" + cfg.Port)
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	<-ctx.Done()
	_ = srv.Close()
}
```

同时更新 `internal/api/router.go`：`NewRouter` 只保留 health 与静态页挂载（业务路由由 main 注入注册）：

```go
// Package api 提供 HTTP 路由装配。MVP 单用户免登录，无认证中间件。
package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// NewRouter 装配基础路由；业务 handler 由 main 调 RegisterXxx 注入。
func NewRouter() *chi.Mux {
	r := chi.NewRouter()
	r.Get("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	// 静态页（Task 15 填充 web/ 目录后生效）
	fileServer := http.FileServer(http.Dir("./web"))
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "./web/index.html")
	})
	r.Handle("/app/*", http.StripPrefix("/app/", fileServer))
	return r
}
```

（`TestHealth` 继续通过。）

- [ ] **Step 4: 运行测试通过**

Run: `make test-integration`
Expected: api 包 PASS

- [ ] **Step 5: 提交**

```bash
git add internal/api/ cmd/zhiwei-server/
git commit -m "feat: 音频上传 API 与服务完整装配"
```

---

### Task 14: 查询 API（sessions / detail / retry）

**Files:**
- Create: `internal/api/query.go`
- Modify: `internal/api/audio.go`（路由注册挪到统一入口，或直接在 query.go 再注册）
- Test: `internal/api/query_test.go`

- [ ] **Step 1: 写失败测试**

`internal/api/query_test.go`：

```go
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

func TestSessionsAndDetail(t *testing.T) {
	db, _ := repo.NewDB(repo.TestDSN(t))
	ctx := context.Background()
	sessions := &repo.SessionRepo{DB: db}
	jobs := &repo.JobRepo{DB: db}
	transcripts := &repo.TranscriptRepo{DB: db}

	sid := ids.New()
	_ = sessions.Create(ctx, &repo.AudioSession{
		ID: sid, Source: "web_upload", Filename: "a.wav",
		StoragePath: "/tmp/a.wav", Status: "completed",
	})
	tr := &repo.Transcript{SessionID: sid, Language: "zh-CN"}
	_ = transcripts.Create(ctx, tr)
	conf := 0.95
	_ = transcripts.InsertSegments(ctx, []repo.TranscriptSegment{
		{TranscriptID: tr.ID, SequenceNo: 1, SpeakerLabel: "1",
			Text: "明天记得发邮件", StartMS: 0, EndMS: 1000, Confidence: &conf},
	})

	handler := setupQueryAPI(t, sessions, jobs, transcripts)

	// 列表
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var listResp struct {
		Sessions []repo.AudioSession `json:"sessions"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &listResp)
	if len(listResp.Sessions) < 1 {
		t.Fatal("sessions 为空")
	}

	// 详情
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/api/sessions/"+sid.String(), nil)
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("detail: %d %s", rec2.Code, rec2.Body.String())
	}
	if !containsAll(rec2.Body.String(), `"segments"`, `"明天记得发邮件"`, `"说话人 1"`) {
		t.Fatalf("detail body = %s", rec2.Body.String())
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
```

（补 `strings` import。`setupQueryAPI` 在实现里提供。）

- [ ] **Step 2: 运行确认失败**

Run: `make test-integration`
Expected: FAIL

- [ ] **Step 3: 实现**

`internal/api/query.go`：

```go
package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// QueryHandler 会话/任务查询。
type QueryHandler struct {
	Sessions    *repo.SessionRepo
	Jobs        *repo.JobRepo
	Transcripts *repo.TranscriptRepo
}

// RegisterQuery 挂载查询路由。
func RegisterQuery(r chi.Router, h *QueryHandler) {
	r.Get("/api/sessions", h.ListSessions)
	r.Get("/api/sessions/{id}", h.GetSession)
	r.Post("/api/jobs/{id}/retry", h.RetryJob)
}

func (h *QueryHandler) ListSessions(w http.ResponseWriter, r *http.Request) {
	limit := intQuery(r, "limit", 50)
	list, err := h.Sessions.List(r.Context(), limit, 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 附带每个 session 最新 job 状态，前端展示处理进度
	type row struct {
		repo.AudioSession
		JobStatus string `json:"job_status,omitempty"`
		JobStage  string `json:"job_stage,omitempty"`
	}
	out := make([]row, len(list))
	for i, s := range list {
		out[i] = row{AudioSession: s}
		if s.JobID != nil {
			if j, err := h.Jobs.Get(r.Context(), *s.JobID); err == nil {
				out[i].JobStatus, out[i].JobStage = j.Status, j.Stage
			}
		}
	}
	writeJSON(w, map[string]any{"sessions": out})
}

type segmentView struct {
	Speaker string `json:"speaker"`
	Text    string `json:"text"`
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
}

func (h *QueryHandler) GetSession(w http.ResponseWriter, r *http.Request) {
	sid, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	s, err := h.Sessions.Get(r.Context(), sid)
	if err != nil {
		http.Error(w, "session 不存在", http.StatusNotFound)
		return
	}
	resp := map[string]any{"session": s}
	if tr, err := h.Transcripts.GetBySession(r.Context(), sid); err == nil {
		segs, _ := h.Transcripts.ListSegments(r.Context(), tr.ID)
		views := make([]segmentView, len(segs))
		for i, sg := range segs {
			views[i] = segmentView{
				Speaker: speakerLabelName(sg.SpeakerLabel), Text: sg.Text,
				StartMS: sg.StartMS, EndMS: sg.EndMS,
			}
		}
		resp["transcript"] = tr
		resp["segments"] = views
	}
	if s.JobID != nil {
		if j, err := h.Jobs.Get(r.Context(), *s.JobID); err == nil {
			resp["job"] = j
		}
	}
	writeJSON(w, resp)
}

func (h *QueryHandler) RetryJob(w http.ResponseWriter, r *http.Request) {
	jid, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	j, err := h.Jobs.Get(r.Context(), jid)
	if err != nil {
		http.Error(w, "job 不存在", http.StatusNotFound)
		return
	}
	if j.Status != "failed" {
		http.Error(w, "仅 failed 状态可重跑", http.StatusConflict)
		return
	}
	j.Status = "pending"
	j.Attempt = 0
	j.LastError = nil
	if err := h.Jobs.Save(r.Context(), j); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"job": j})
}

func speakerLabelName(label string) string {
	if label == "" {
		return "未知说话人"
	}
	return "说话人 " + label
}

func intQuery(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 || n > 200 {
		return def
	}
	return n
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
```

`internal/ids/ids.go` 追加解析函数：

```go
// ParseID 从 URL 路径参数解析 ID（仅数字）。
func ParseID(s string) (ID, error) {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid id: %s", s)
	}
	return ID(v), nil
}
```

（`fmt` import 补上。）

`stage_asr.go` 里的 `speakerName` 与本文件 `speakerLabelName` 重复——删除 `stage_asr.go` 中的 `speakerName`，统一用 api 层的展示函数；`stageSegment` 内改用内联 `"说话人 " + label`（空标签走 `speakerLabelName` 同逻辑，直接把该函数移到 ids 无关的 `internal/pipeline` 也删掉，保留 api 版本）。

main.go 装配追加：

```go
	api.RegisterQuery(r, &api.QueryHandler{
		Sessions: sessions, Jobs: jobs, Transcripts: transcripts,
	})
```

测试里的 `setupQueryAPI`（放在 `query_test.go`）：

```go
func setupQueryAPI(t *testing.T, s *repo.SessionRepo, j *repo.JobRepo, tr *repo.TranscriptRepo) http.Handler {
	t.Helper()
	_ = ids.Init(1)
	r := chi.NewRouter()
	RegisterQuery(r, &QueryHandler{Sessions: s, Jobs: j, Transcripts: tr})
	return r
}
```

注意删除 query_test.go 中对 `pipeline` 的 import（未用到）。

- [ ] **Step 4: 运行测试通过**

Run: `make test-integration`
Expected: api 包全部 PASS

- [ ] **Step 5: 提交**

```bash
git add internal/api/ internal/ids/ cmd/zhiwei-server/
git commit -m "feat: 会话列表/详情与 job 重跑 API"
```

---

### Task 15: Web UI（时间线 + 录音）

**Files:**
- Create: `web/index.html`、`web/vendor/vue.global.prod.js`
- Modify: 无（静态路由 Task 13 已挂）

- [ ] **Step 1: vendor Vue 产物（国内镜像，无构建步骤）**

```bash
mkdir -p web/vendor
curl -fsSL https://registry.npmmirror.com/vue/3.4.38/files/dist/vue.global.prod.js -o web/vendor/vue.global.prod.js
ls -la web/vendor/
```

Expected: 文件 > 100KB。若镜像失败，换 `https://unpkg.com/vue@3.4.38/dist/vue.global.prod.js`。

- [ ] **Step 2: 写单页应用**

`web/index.html`：

```html
<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>知微</title>
<script src="/app/vendor/vue.global.prod.js"></script>
<style>
  * { box-sizing: border-box; margin: 0; }
  body { font-family: -apple-system, "PingFang SC", sans-serif; background: #f6f7f9; color: #222; }
  .tabs { display: flex; background: #fff; border-bottom: 1px solid #e5e7eb; padding: 0 16px; }
  .tabs button { padding: 14px 18px; border: none; background: none; font-size: 15px; cursor: pointer; color: #6b7280; }
  .tabs button.active { color: #111; border-bottom: 2px solid #111; font-weight: 600; }
  .wrap { max-width: 760px; margin: 0 auto; padding: 16px; }
  .card { background: #fff; border-radius: 12px; padding: 14px 16px; margin-bottom: 12px; box-shadow: 0 1px 2px rgba(0,0,0,.04); }
  .muted { color: #9ca3af; font-size: 13px; }
  .badge { display: inline-block; padding: 2px 8px; border-radius: 99px; font-size: 12px; }
  .badge.pending, .badge.running { background: #fef3c7; color: #92400e; }
  .badge.completed, .badge.done { background: #d1fae5; color: #065f46; }
  .badge.failed { background: #fee2e2; color: #991b1b; }
  .seg { padding: 8px 0; border-bottom: 1px dashed #f0f0f0; }
  .seg:last-child { border: none; }
  .sp { font-size: 12px; color: #fff; border-radius: 6px; padding: 1px 6px; margin-right: 8px; }
  .sp1 { background: #6366f1; } .sp2 { background: #10b981; } .sp3 { background: #f59e0b; }
  button.primary { background: #111; color: #fff; border: none; border-radius: 10px; padding: 10px 22px; font-size: 15px; cursor: pointer; }
  button.primary:disabled { background: #9ca3af; }
  #drop { border: 2px dashed #d1d5db; border-radius: 12px; padding: 36px; text-align: center; color: #6b7280; margin-bottom: 12px; }
  #drop.rec { border-color: #ef4444; color: #ef4444; }
</style>
</head>
<body>
<div id="app">
  <div class="tabs">
    <button :class="{active: tab==='timeline'}" @click="tab='timeline'; loadSessions()">时间线</button>
    <button :class="{active: tab==='record'}" @click="tab='record'">录音</button>
  </div>

  <!-- 时间线 -->
  <div class="wrap" v-if="tab==='timeline'">
    <div v-if="!sessions.length" class="card muted">还没有记录。去「录音」页上传或录一段吧。</div>
    <div class="card" v-for="s in sessions" :key="s.id" style="cursor:pointer" @click="openSession(s.id)">
      <div style="display:flex; justify-content:space-between; align-items:center">
        <div>
          <b>{{ s.filename }}</b>
          <div class="muted">{{ fmtTime(s.created_at) }} · {{ s.source === 'web_record' ? '录音' : '上传' }}</div>
        </div>
        <span class="badge" :class="s.job_status || s.status">
          {{ statusText(s.job_status, s.job_stage) }}
        </span>
      </div>
    </div>

    <!-- 详情弹层 -->
    <div v-if="detail" class="card">
      <div style="display:flex; justify-content:space-between">
        <b>转写详情</b>
        <button class="muted" style="border:none;background:none;cursor:pointer" @click="detail=null">✕ 关闭</button>
      </div>
      <div class="muted" style="margin:6px 0">{{ detail.session.filename }}</div>
      <div v-if="detail.job && detail.job.status==='failed'" style="margin:8px 0">
        <span class="badge failed">处理失败</span>
        <span class="muted">{{ detail.job.last_error }}</span>
        <button class="primary" style="padding:4px 12px" @click="retryJob(detail.job.id)">重跑</button>
      </div>
      <div v-for="(sg, i) in detail.segments" :key="i" class="seg">
        <span class="sp" :class="spClass(sg.speaker)">{{ sg.speaker }}</span>{{ sg.text }}
      </div>
    </div>
  </div>

  <!-- 录音 -->
  <div class="wrap" v-if="tab==='record'">
    <div id="drop" :class="{rec: recording}"
         @dragover.prevent @drop.prevent="onDrop">
      <template v-if="!recording">拖拽音频文件到此处，或点击下方按钮录音</template>
      <template v-else>● 录音中…（{{ recSeconds }}s）</template>
    </div>
    <div style="display:flex; gap:12px">
      <button class="primary" v-if="!recording" @click="startRec">开始录音</button>
      <button class="primary" v-else @click="stopRec">停止并上传</button>
    </div>
    <div v-if="uploadInfo" class="card" style="margin-top:12px">
      <div class="muted">{{ uploadInfo.filename }}</div>
      <span class="badge" :class="uploadInfo.status">{{ uploadInfo.text }}</span>
    </div>
  </div>
</div>

<script>
const { createApp, ref, onUnmounted } = Vue;

createApp({
  setup() {
    const tab = ref('timeline');
    const sessions = ref([]);
    const detail = ref(null);
    const recording = ref(false);
    const recSeconds = ref(0);
    const uploadInfo = ref(null);
    let recorder = null, recTimer = null, pollTimer = null;

    async function loadSessions() {
      const r = await fetch('/api/sessions');
      const d = await r.json();
      sessions.value = d.sessions || [];
    }
    loadSessions();

    async function openSession(id) {
      const r = await fetch('/api/sessions/' + id);
      detail.value = await r.json();
    }

    async function retryJob(id) {
      await fetch('/api/jobs/' + id + '/retry', { method: 'POST' });
      await openSession(detail.value.session.id);
    }

    function statusText(status, stage) {
      if (status === 'done' || status === 'completed') return '已完成';
      if (status === 'failed') return '失败';
      if (status === 'running') return '处理中 · ' + stage;
      return '排队中';
    }
    function spClass(speaker) {
      const n = (speaker || '').replace(/\D/g, '') || '1';
      return 'sp' + Math.min(Number(n), 3);
    }
    function fmtTime(iso) { return new Date(iso).toLocaleString('zh-CN'); }

    // ---- 录音与上传 ----
    async function startRec() {
      const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
      recorder = new MediaRecorder(stream, { mimeType: 'audio/webm;codecs=opus' });
      const chunks = [];
      recorder.ondataavailable = e => chunks.push(e.data);
      recorder.onstop = () => {
        stream.getTracks().forEach(t => t.stop());
        upload(new File(chunks, 'record-' + Date.now() + '.webm', { type: 'audio/webm' }), 'web_record');
      };
      recorder.start();
      recording.value = true; recSeconds.value = 0;
      recTimer = setInterval(() => recSeconds.value++, 1000);
    }
    function stopRec() {
      recorder.stop(); recording.value = false;
      clearInterval(recTimer);
    }
    function onDrop(e) {
      const f = e.dataTransfer.files[0];
      if (f) upload(f, 'web_upload');
    }
    async function upload(file, source) {
      const fd = new FormData();
      fd.append('file', file); fd.append('source', source);
      uploadInfo.value = { filename: file.name, status: 'pending', text: '上传中…' };
      const r = await fetch('/api/audio', { method: 'POST', body: fd });
      const d = await r.json();
      if (!r.ok) { uploadInfo.value = { filename: file.name, status: 'failed', text: d.error || '上传失败' }; return; }
      uploadInfo.value = { filename: file.name, status: 'running', text: '已上传，处理中…', sessionId: d.session_id };
      pollTimer = setInterval(async () => {
        const rr = await fetch('/api/sessions/' + d.session_id);
        const dd = await rr.json();
        const st = dd.job ? dd.job.status : dd.session.status;
        if (st === 'done' || st === 'completed') {
          clearInterval(pollTimer);
          uploadInfo.value = { filename: file.name, status: 'done', text: '处理完成 ✓' };
          loadSessions();
        } else if (st === 'failed') {
          clearInterval(pollTimer);
          uploadInfo.value = { filename: file.name, status: 'failed', text: '处理失败，可在时间线重跑' };
        }
      }, 2000);
    }

    onUnmounted(() => { clearInterval(recTimer); clearInterval(pollTimer); });

    return { tab, sessions, detail, recording, recSeconds, uploadInfo,
             loadSessions, openSession, retryJob, statusText, spClass, fmtTime,
             startRec, stopRec, onDrop };
  }
}).mount('#app');
</script>
</body>
</html>
```

- [ ] **Step 3: 手动验证**

```bash
make compose-up && make migrate-up
make dev
# 浏览器打开 http://localhost:8080
# 1. 录音页：说一句「明天记得给 Tom 发邮件」，停止上传
# 2. 观察状态从 排队中 → 处理中(asr) → 处理中(segment) → 已完成
# 3. 时间线点开卡片：能看到带说话人颜色的转写文本
```

Expected: 全链路展示正常，转写文本可见。

- [ ] **Step 4: 提交**

```bash
git add web/
git commit -m "feat: Web 时间线与录音页（Vue 3 无构建单页）"
```

---

### Task 16: e2e 冒烟与验收

**Files:**
- Create: `scripts/e2e.sh`
- Modify: `Makefile`（e2e 目标）

- [ ] **Step 1: 写 e2e 脚本**

`scripts/e2e.sh`：

```bash
#!/usr/bin/env bash
# e2e 冒烟：起依赖 → 起服务 → 上传音频 → 轮询到终态 → 校验转写非空
set -euo pipefail

cd "$(dirname "$0")/.."

echo "==> 启动 MySQL 并迁移"
make compose-up
sleep 5
make migrate-up || true   # 已迁移则跳过

echo "==> 启动 zhiwei-server"
make build
./bin/zhiwei-server &
SERVER_PID=$!
trap 'kill $SERVER_PID 2>/dev/null || true' EXIT
sleep 2

echo "==> 健康检查"
curl -fsS localhost:8080/api/health

echo "==> 上传音频"
RESP=$(curl -fsS -F "file=@testdata/sample.wav" -F "source=web_upload" localhost:8080/api/audio)
echo "$RESP"
SESSION_ID=$(echo "$RESP" | sed -E 's/.*"session_id":"([0-9]+)".*/\1/')

echo "==> 轮询处理结果（最多 120s）"
for i in $(seq 1 60); do
  DETAIL=$(curl -fsS "localhost:8080/api/sessions/$SESSION_ID")
  STATUS=$(echo "$DETAIL" | grep -o '"job":{[^}]*}' | grep -o '"status":"[a-z]*"' | head -1 | cut -d'"' -f4 || true)
  echo "  [$i] status=$STATUS"
  if [ "$STATUS" = "done" ]; then
    if echo "$DETAIL" | grep -q '"segments":\[\]'; then
      echo "FAIL: segments 为空"; exit 1
    fi
    echo "PASS: pipeline 跑通，转写已生成"; exit 0
  fi
  if [ "$STATUS" = "failed" ]; then
    echo "FAIL: 处理失败"; echo "$DETAIL"; exit 1
  fi
  sleep 2
done
echo "FAIL: 120s 未完成"; exit 1
```

- [ ] **Step 2: Makefile 目标 + 可执行权限**

```makefile
e2e:
	bash scripts/e2e.sh
```

```bash
chmod +x scripts/e2e.sh
```

- [ ] **Step 3: 运行 e2e**

```bash
make e2e
```

Expected: `PASS: pipeline 跑通，转写已生成`（用 sine 波时 segments 可能为空——属预期失败路径，**最终验收必须换真人语音文件**跑一次）。

- [ ] **Step 4: 真人语音验收（Sprint 0-1 Done 标准）**

用手机录一段两人对话（30 秒+，内容包含明确待办，如「明天记得给 Tom 发邮件」），替换 testdata 后跑：

```bash
curl -fsS -F "file=@testdata/real.wav" -F "source=web_upload" localhost:8080/api/audio | tee /tmp/up.json
# 轮询完成
curl -fsS "localhost:8080/api/sessions/$(jq -r .session_id /tmp/up.json)" | jq .
```

验收清单：
- [ ] 转写文本正确包含关键语句
- [ ] 不同说话人 segment 的 speaker 标签不同且分色展示
- [ ] 时间线列表状态流转正确
- [ ] `make test` 与 `make test-integration` 全绿

- [ ] **Step 5: 提交**

```bash
git add scripts/ Makefile
git commit -m "test: e2e 冒烟脚本与验收标准"
```

---

## Sprint 0-1 完成后

系统状态：上传/录音 → ASR（说话人分离）→ 转写时间线 全链路可用。

下一步：基于本计划的实现情况与 asr-protocol-notes.md，编写 Sprint 2 计划（Memory 抽取 / Todo / Topic 组织层），届时扩展 `Flow.Stages` 为 `asr, segment, extract, quality, commit` 并复用全部基础设施。
