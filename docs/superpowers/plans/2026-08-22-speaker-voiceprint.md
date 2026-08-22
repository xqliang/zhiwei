# 说话人声纹识别与检索 · 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 为知微云端 MVP 增加根据语音识别说话人的能力——StepFun 异步文件 ASR 出原生时间戳+说话人，ffmpeg 切片，本地 WeSpeaker 提 256 维声纹，FAISS 做 1:N 检索/自动登记，时间线可点击说话人过滤/改名/换人/录入。

**Architecture:** ASR 改用 StepFun 异步文件接口（原生 ms 时间戳 + spk_N diarization），音频经火山引擎 TOS presigned URL 喂给 StepFun。新增 Python FastAPI sidecar 承载 WeSpeaker resnet34-LM + FAISS IndexIDMap2(IndexFlatIP)。新增 `speaker` stage（插在 segment 与 extract 之间）：按 ASR 说话人分组→切片→提向→聚合→1:N 检索/登记→回填 `transcript_segment.speaker_id`。MySQL `speaker` 表存名册+向量 BLOB 灾备。

**Tech Stack:** Go 1.25（chi + sqlx + MySQL + worker pool）、火山引擎 TOS Go SDK、Python（FastAPI + faiss-cpu + WeSpeaker/onnxruntime）、Vue3 vendor 前端、ffmpeg、golang-migrate。

**Spec:** `docs/superpowers/specs/2026-08-22-speaker-voiceprint-design.md`

---

## 文件结构

| 文件 | 职责 |
|---|---|
| `migrations/000004_speaker.up.sql` / `.down.sql` | speaker 表 + transcript_segment.speaker_id |
| `internal/repo/speaker.go` | speaker 表 CRUD + session 说话人聚合查询 |
| `internal/repo/transcript.go`（改） | 增 SetSegmentSpeaker / SetSegmentSpeakerByID / ListSpeakersForTranscript |
| `internal/provider/asr.go`（改） | 新增 StepFunFileASR + parseFileASRResult；保留旧 StepFunASR |
| `internal/storage/tos.go`（新） | TOS 上传/presign/删除 |
| `internal/voiceprint/client.go`（新） | sidecar HTTP 客户端接口 + 实现 |
| `services/voiceprint/app.py`（新） | FastAPI sidecar：WeSpeaker + FAISS |
| `services/voiceprint/spike.py`（新） | 验证 WeSpeaker 加载 + 256 维 |
| `services/voiceprint/test_app.py`（新） | stub embedder 的 pytest |
| `services/voiceprint/requirements.txt`（新） | Python 依赖 |
| `internal/pipeline/stage_speaker.go`（新） | speaker 解析 stage |
| `internal/pipeline/stage_asr.go`（改） | ASR stage 用 StepFunFileASR + TOS |
| `internal/pipeline/state.go`/`pool.go`（改） | Flow 增 speaker stage、StageDeps 增依赖 |
| `internal/api/speaker.go`（新） | speaker API handler |
| `internal/api/query.go`（改） | GetSession 增强 segment.speaker + speakers[] |
| `internal/api/router.go`（改）/ `cmd/zhiwei-server/main.go`（改） | 装配 |
| `internal/config/config.go`（改） | TOS/sidecar/阈值配置 |
| `cmd/spike/tos/main.go`（新） | TOS SDK 验证 spike |
| `cmd/spike/asr/main.go`（改） | 改为文件 ASR 端到端 spike |
| `web/app.js` / `web/index.html`（改） | 说话人面板 + 录入 + 换人 |
| `Makefile`（改）/ `README.md`（改） | sidecar/spike 目标 + env 说明 |

---

## Phase 0：Spike——验证外部 API

真实 API（TOS Go SDK、WeSpeaker Python）签名不确定，先跑 spike 拿到可用调用，再固化客户端。沿用项目现有 spike 模式（`cmd/spike/llm`、`embed`、`asr`，手动跑、不进 CI）。

### Task 1：TOS Go SDK spike

**Files:**
- Create: `cmd/spike/tos/main.go`
- Create: `go.mod`（改，加依赖）

- [ ] **Step 1: 加 TOS Go SDK 依赖**

Run: `go get github.com/volcengine/ve-tos-golang-sdk/v2`
Expected: `go.mod` 出现 `github.com/volcengine/ve-tos-golang-sdk/v2`。

- [ ] **Step 2: 写 spike**

`cmd/spike/tos/main.go`：

```go
// spike: 验证火山引擎 TOS Go SDK 的上传/presign/删除三件套。
// 用法: TOS_ACCESS_KEY=.. TOS_SECRET_KEY=.. go run ./cmd/spike/tos <本地wav>
// 目的: 拿到可用的 NewClientV2 / PutObjectFromFile / PreSignedURL / DeleteObjectV2 调用，
//       确认输入结构体字段名（Bucket/Key/FilePath/HTTPMethod/Expires 等），固化到 internal/storage/tos.go。
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/volcengine/ve-tos-golang-sdk/v2/tos"
)

func main() {
	ak, sk := os.Getenv("TOS_ACCESS_KEY"), os.Getenv("TOS_SECRET_KEY")
	if ak == "" || sk == "" || len(os.Args) < 2 {
		fmt.Println("用法: TOS_ACCESS_KEY=.. TOS_SECRET_KEY=.. go run ./cmd/spike/tos <wav>")
		os.Exit(1)
	}
	localPath := os.Args[1]
	endpoint := "tos-cn-shanghai.volces.com"
	region := "cn-shanghai"
	bucket := "user-growth"
	key := "zhiwei/spike-" + fmt.Sprint(time.Now().Unix()) + ".wav"

	client, err := tos.NewClientV2(endpoint, tos.WithRegion(region),
		tos.WithCredentialsProvider(tos.NewStaticCredentialsProvider(ak, sk, "")))
	if err != nil {
		panic(err)
	}
	ctx := context.Background()

	// 上传（私有，默认 ACL）
	_, err = client.PutObjectFromFile(ctx, &tos.PutObjectFromFileInput{
		Bucket: bucket, Key: key, FilePath: localPath, ContentType: "audio/wav",
	})
	if err != nil {
		panic(fmt.Errorf("上传: %w", err))
	}
	fmt.Println("uploaded:", key)

	// presigned GET URL（1h）
	out, err := client.PreSignedURL(ctx, &tos.PreSignedURLInput{
		Bucket: bucket, Key: key, HTTPMethod: tos.HTTPMethodGet, Expires: 3600,
	})
	if err != nil {
		panic(fmt.Errorf("presign: %w", err))
	}
	fmt.Println("presigned:", out.SignedUrl)

	// 验证 URL 可拉取（打印 HEAD 状态码）
	resp, err := http.Head(out.SignedUrl) // 需 import "net/http"
	if err == nil {
		fmt.Println("fetch status:", resp.StatusCode)
		resp.Body.Close()
	}

	// 删除
	_, err = client.DeleteObjectV2(ctx, &tos.DeleteObjectV2Input{Bucket: bucket, Key: key})
	if err != nil {
		fmt.Println("delete err:", err)
	}
	fmt.Println("done")
}
```

> 注意：字段名（`Bucket`/`Key`/`FilePath`/`HTTPMethod`/`Expires`/`SignedUrl`）取自 pkg.go.dev 索引。若编译报字段不存在，查 `https://pkg.go.dev/github.com/volcengine/ve-tos-golang-sdk/v2/tos` 对应结构体修正，并在本 spike 注释里记下真实字段名。

- [ ] **Step 3: 跑 spike 验证**

Run: `TOS_ACCESS_KEY=.. TOS_SECRET_KEY=.. go run ./cmd/spike/tos testdata/speech.wav`
Expected: 打印 `uploaded` / `presigned: https://...` / `fetch status: 200` / `done`。若字段名不对，按 Step 2 注释修。

- [ ] **Step 4: 记录真实 API + 提交**

把 spike 跑通后确认的 `NewClientV2`/`PutObjectFromFile`/`PreSignedURL`/`DeleteObjectV2` 真实字段名写到 `cmd/spike/tos/main.go` 顶部注释，提交。

```bash
git add cmd/spike/tos/main.go go.mod go.sum
git commit -m "spike: 验证 TOS Go SDK 上传/presign/删除"
```

### Task 2：WeSpeaker Python spike

**Files:**
- Create: `services/voiceprint/requirements.txt`
- Create: `services/voiceprint/spike.py`

- [ ] **Step 1: 写依赖**

`services/voiceprint/requirements.txt`：

```
fastapi
uvicorn[standard]
faiss-cpu
numpy
# WeSpeaker 推理依赖（spike 确认确切包名后固定）
onnxruntime
```

- [ ] **Step 2: 写 spike**

`services/voiceprint/spike.py`：

```python
"""spike: 验证 WeSpeaker resnet34-LM 加载 + 提取 256 维声纹。
用法: python services/voiceprint/spike.py <wav>
目的: 拿到可用的模型加载 + embedding 提取调用，确认输出维度=256，
      固化到 services/voiceprint/embedder.py。
WeSpeaker 仓库: https://github.com/wenet-org/wespeaker
参考其 README 的 Python/ONNX 推理段落；若 API 不同，按实际修正并在注释记录。
"""
import sys
import numpy as np

def load_embedder():
    # TODO(spike): 按 WeSpeaker 实际 API 填充。
    # 候选: ONNX Runtime 加载 resnet34 ONNX；或 wespeaker python 包。
    # 返回一个对象，含 embed(wav_path)->np.ndarray(256,) 方法。
    raise NotImplementedError("spike: 填充 WeSpeaker 加载逻辑")

def main():
    if len(sys.argv) < 2:
        print("用法: python services/voiceprint/spike.py <wav>"); sys.exit(1)
    emb = load_embedder()
    vec = emb.embed(sys.argv[1])
    print("dim:", vec.shape)
    assert vec.shape == (256,), f"期望 256 维，实际 {vec.shape}"
    # L2 归一
    vec = vec / (np.linalg.norm(vec) + 1e-12)
    print("norm:", float(np.linalg.norm(vec)))
    print("ok")

if __name__ == "__main__":
    main()
```

- [ ] **Step 3: 跑 spike 验证**

Run: `python services/voiceprint/spike.py testdata/speech.wav`
Expected: `dim: (256,)` / `norm: 1.0` / `ok`。按 WeSpeaker README 填 `load_embedder`，确认 256 维。

- [ ] **Step 4: 提交**

```bash
git add services/voiceprint/requirements.txt services/voiceprint/spike.py
git commit -m "spike: 验证 WeSpeaker resnet34-LM 输出 256 维"
```

### Task 3：StepFun 文件 ASR spike

**Files:**
- Modify: `cmd/spike/asr/main.go`

- [ ] **Step 1: 改写 ASR spike 为文件接口端到端**

`cmd/spike/asr/main.go`：

```go
// spike: 验证 StepFun 异步文件 ASR 端到端（TOS 上传→submit→query→解析 utterances）。
// 用法: STEPFUN_API_KEY=.. TOS_ACCESS_KEY=.. TOS_SECRET_KEY=.. go run ./cmd/spike/asr <wav>
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("用法: STEPFUN_API_KEY=.. TOS_ACCESS_KEY=.. TOS_SECRET_KEY=.. go run ./cmd/spike/asr <wav>"); os.Exit(1)
	}
	// 复用 internal/storage/tos.go 的上传/presign（Task 7 完成后可直接 import）；
	// 此 spike 阶段可先手动用 Task 1 的 spike 上传拿 URL，或直接 import 已实现的 TOSClient。
	localPath := os.Args[1]
	tosURL := mustUploadAndPresign(localPath) // Task 1 spike 产出的逻辑；此处先内联或 import

	body, _ := json.Marshal(map[string]any{
		"audio":   map[string]any{"format": "wav", "channel": 1, "rate": 16000, "url": tosURL},
		"request": map[string]any{"model_name": "stepaudio-2.5-asr", "show_utterances": true, "enable_speaker_info": true},
	})
	submitURL := "https://api.stepfun.com/v1/audio/asr/file/submit"
	req, _ := http.NewRequestWithContext(context.Background(), "POST", submitURL, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+os.Getenv("STEPFUN_API_KEY"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil { panic(err) }
	defer resp.Body.Close()
	var sub struct{ TaskID string `json:"task_id"` }
	b, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(b, &sub); err != nil || sub.TaskID == "" {
		panic(fmt.Errorf("submit 失败: %s", b))
	}
	fmt.Println("task:", sub.TaskID)

	// 轮询 query
	for i := 0; i < 150; i++ {
		time.Sleep(2 * time.Second)
		qb, _ := json.Marshal(map[string]string{"task_id": sub.TaskID})
		qreq, _ := http.NewRequestWithContext(context.Background(), "POST",
			"https://api.stepfun.com/v1/audio/asr/file/query", bytes.NewReader(qb))
		qreq.Header.Set("Authorization", "Bearer "+os.Getenv("STEPFUN_API_KEY"))
		qreq.Header.Set("Content-Type", "application/json")
		qresp, err := http.DefaultClient.Do(qreq)
		if err != nil { continue }
		var qr struct {
			Status   string `json:"status"`
			Duration float64 `json:"duration"`
			Result   []struct {
				Text       string `json:"text"`
				Utterances []struct {
					Text      string `json:"text"`
					StartTime int    `json:"start_time"`
					EndTime   int    `json:"end_time"`
					Speaker   struct{ ID string `json:"id"` } `json:"speaker"`
				} `json:"utterances"`
			} `json:"result"`
		}
		rb, _ := io.ReadAll(qresp.Body); qresp.Body.Close()
		_ = json.Unmarshal(rb, &qr)
		fmt.Println("status:", qr.Status)
		if qr.Status == "FAILED" { panic("asr failed: " + string(rb)) }
		if qr.Status != "PENDING" && qr.Status != "RUNNING" && len(qr.Result) > 0 {
			for _, r := range qr.Result {
				for _, u := range r.Utterances {
					fmt.Printf("[%s] %d-%dms %s\n", u.Speaker.ID, u.StartTime, u.EndTime, u.Text)
				}
			}
			fmt.Println("duration(s):", qr.Duration)
			return
		}
	}
	panic("timeout")
}

func mustUploadAndPresign(localPath string) string {
	// 见 Task 1 spike；此处省略，实际 import internal/storage TOSClient 或内联
	return ""
}
```

- [ ] **Step 2: 跑 spike 验证**

Run: `STEPFUN_API_KEY=.. TOS_ACCESS_KEY=.. TOS_SECRET_KEY=.. go run ./cmd/spike/asr testdata/speech.wav`
Expected: 打印 `task: <id>` → 轮询 `status: RUNNING` → 完成后逐句 `[spk_1] 0-2340ms 你好`。

- [ ] **Step 3: 提交**

```bash
git add cmd/spike/asr/main.go
git commit -m "spike: StepFun 异步文件 ASR 端到端验证"
```

---

## Phase 1：数据模型

### Task 4：speaker 表迁移 + SpeakerRepo

**Files:**
- Create: `migrations/000004_speaker.up.sql`
- Create: `migrations/000004_speaker.down.sql`
- Create: `internal/repo/speaker.go`
- Create: `internal/repo/speaker_test.go`

- [ ] **Step 1: 写迁移 up**

`migrations/000004_speaker.up.sql`：

```sql
CREATE TABLE speaker (
  id           BIGINT PRIMARY KEY,
  user_id      BIGINT NOT NULL DEFAULT 1,
  name         VARCHAR(128) NOT NULL,
  source       VARCHAR(8) NOT NULL DEFAULT 'auto',
  status       VARCHAR(16) NOT NULL DEFAULT 'active',
  embedding    LONGBLOB NULL,
  sample_count INT NOT NULL DEFAULT 0,
  created_at   DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at   DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  KEY idx_user_status (user_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

ALTER TABLE transcript_segment ADD COLUMN speaker_id BIGINT NULL AFTER speaker_label;
ALTER TABLE transcript_segment ADD KEY idx_speaker (speaker_id);
```

- [ ] **Step 2: 写迁移 down**

`migrations/000004_speaker.down.sql`：

```sql
ALTER TABLE transcript_segment DROP KEY idx_speaker;
ALTER TABLE transcript_segment DROP COLUMN speaker_id;
DROP TABLE IF EXISTS speaker;
```

- [ ] **Step 3: 跑迁移验证**

Run: `make migrate-up`
Expected: 无错误。`make init-testdb` 后 `zhiwei_test` 库出现 `speaker` 表 + `transcript_segment.speaker_id` 列。

- [ ] **Step 4: 写 SpeakerRepo（先写测试）**

`internal/repo/speaker_test.go`：

```go
package repo

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
)

func TestSpeakerCRUD(t *testing.T) {
	if testing.Short() { t.Skip("需要 MySQL") }
	ctx := context.Background()
	r := &SpeakerRepo{DB: testDB} // testDB 见 main_test.go

	sp := &Speaker{Name: "说话人ab12c", Source: "auto", Embedding: []byte{0, 0, 0, 0}, SampleCount: 1}
	if err := r.Create(ctx, sp); err != nil { t.Fatalf("create: %v", err) }
	if sp.ID == 0 { t.Fatal("id 未回填") }

	got, err := r.Get(ctx, sp.ID)
	if err != nil { t.Fatalf("get: %v", err) }
	if got.Name != "说话人ab12c" || got.Source != "auto" { t.Fatalf("got %+v", got) }

	if err := r.UpdateName(ctx, sp.ID, "张三"); err != nil { t.Fatalf("updateName: %v", err) }
	if g2, _ := r.Get(ctx, sp.ID); g2.Name != "张三" { t.Fatalf("name=%s", g2.Name) }

	list, err := r.List(ctx)
	if err != nil || len(list) == 0 { t.Fatalf("list: %v %d", err, len(list)) }

	if err := r.Delete(ctx, sp.ID); err != nil { t.Fatalf("delete: %v", err) }
	if _, err := r.Get(ctx, sp.ID); err == nil { t.Fatal("删除后仍可查到") }
}

func TestSpeakerRepoGetBySession(t *testing.T) {
	if testing.Short() { t.Skip("需要 MySQL") }
	// 在 ListSpeakersForTranscript 测试里覆盖（Task 11 联动）；此处占位保证包可编译
	_ = ids.ID(0)
}
```

- [ ] **Step 5: 跑测试验证失败**

Run: `TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./internal/repo/ -run TestSpeaker -v`
Expected: FAIL（`SpeakerRepo`/`Speaker` 未定义）。

- [ ] **Step 6: 写 SpeakerRepo 实现**

`internal/repo/speaker.go`：

```go
package repo

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"

	"zhiwei/internal/ids"
)

// Speaker 说话人声纹名册。向量实际存 FAISS，embedding BLOB 作灾备/可重建索引。
type Speaker struct {
	ID          ids.ID    `db:"id" json:"id"`
	UserID      int64     `db:"user_id" json:"user_id"`
	Name        string    `db:"name" json:"name"`
	Source      string    `db:"source" json:"source"`             // enrolled | auto
	Status      string    `db:"status" json:"status"`             // active | dismissed
	Embedding   []byte    `db:"embedding" json:"-"`               // 256×float32=1024B，不外泄
	SampleCount int       `db:"sample_count" json:"sample_count"`
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time `db:"updated_at" json:"updated_at"`
}

type SpeakerRepo struct{ DB *sqlx.DB }

func (r *SpeakerRepo) Create(ctx context.Context, s *Speaker) error {
	s.ID = ids.New()
	if s.UserID == 0 { s.UserID = 1 }
	if s.Source == "" { s.Source = "auto" }
	if s.Status == "" { s.Status = "active" }
	_, err := r.DB.NamedExecContext(ctx, `
INSERT INTO speaker (id, user_id, name, source, status, embedding, sample_count)
VALUES (:id, :user_id, :name, :source, :status, :embedding, :sample_count)`, s)
	return err
}

func (r *SpeakerRepo) Get(ctx context.Context, id ids.ID) (*Speaker, error) {
	var s Speaker
	err := r.DB.GetContext(ctx, &s, `SELECT * FROM speaker WHERE id = ?`, id.Int64())
	return &s, err
}

// List 全部 active 说话人（管理页/换人下拉用）。
func (r *SpeakerRepo) List(ctx context.Context) ([]Speaker, error) {
	var list []Speaker
	err := r.DB.SelectContext(ctx, &list,
		`SELECT * FROM speaker WHERE status = 'active' ORDER BY id DESC`)
	return list, err
}

func (r *SpeakerRepo) UpdateName(ctx context.Context, id ids.ID, name string) error {
	_, err := r.DB.ExecContext(ctx, `UPDATE speaker SET name = ? WHERE id = ?`, name, id.Int64())
	return err
}

// UpdateEmbedding 重新登记/增量更新向量时同步 BLOB（灾备）。
func (r *SpeakerRepo) UpdateEmbedding(ctx context.Context, id ids.ID, emb []byte) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE speaker SET embedding = ? WHERE id = ?`, emb, id.Int64())
	return err
}

func (r *SpeakerRepo) Delete(ctx context.Context, id ids.ID) error {
	_, err := r.DB.ExecContext(ctx, `DELETE FROM speaker WHERE id = ?`, id.Int64())
	return err
}
```

- [ ] **Step 7: 跑测试验证通过**

Run: `TEST_MYSQL_DSN="..." go test ./internal/repo/ -run TestSpeakerCRUD -v`
Expected: PASS。

- [ ] **Step 8: 提交**

```bash
git add migrations/000004_speaker.* internal/repo/speaker.go internal/repo/speaker_test.go
git commit -m "feat(repo): speaker 表迁移 + SpeakerRepo"
```

---

## Phase 2：ASR 重写

### Task 5：parseFileASRResult 纯函数

**Files:**
- Modify: `internal/provider/asr.go`（加函数）
- Modify: `internal/provider/asr_test.go`（加测试）

- [ ] **Step 1: 写失败测试**

`internal/provider/asr_test.go` 末尾加：

```go
func TestParseFileASRResult(t *testing.T) {
	raw := []byte(`{
	  "status":"SUCCEEDED","duration":6.2,
	  "result":[{
	    "text":"你好。我咨询一下。",
	    "utterances":[
	      {"text":"你好。","start_time":2000,"end_time":4500,"speaker":{"id":"spk_1"}},
	      {"text":"我咨询一下。","start_time":4500,"end_time":6200,"speaker":{"id":"spk_2"}}
	    ]
	  }]
	}`)
	pieces := ParseFileASRResult(raw)
	if len(pieces) != 2 { t.Fatalf("len=%d", len(pieces)) }
	if pieces[0].SpeakerLabel != "1" { t.Fatalf("label=%q", pieces[0].SpeakerLabel) }
	if pieces[0].StartMS != 2000 || pieces[0].EndMS != 4500 { t.Fatalf("ms=%d-%d", pieces[0].StartMS, pieces[0].EndMS) }
	if pieces[1].SpeakerLabel != "2" || pieces[1].Text != "我咨询一下。" { t.Fatalf("p1=%+v", pieces[1]) }
}

func TestParseFileASRResultNoSpeaker(t *testing.T) {
	// 未开 speaker_info：speaker 字段缺失，label 空
	raw := []byte(`{"status":"SUCCEEDED","result":[{"text":"x","utterances":[
	  {"text":"x","start_time":0,"end_time":100}]}]}`)
	pieces := ParseFileASRResult(raw)
	if len(pieces) != 1 || pieces[0].SpeakerLabel != "" || pieces[0].StartMS != 0 {
		t.Fatalf("%+v", pieces)
	}
}
```

- [ ] **Step 2: 跑测试验证失败**

Run: `go test ./internal/provider/ -run TestParseFileASR -v`
Expected: FAIL（`ParseFileASRResult` undefined）。

- [ ] **Step 3: 写实现**

`internal/provider/asr.go` 末尾加：

```go
// fileASRQueryResponse 对应 StepFun 异步文件 ASR /file/query 响应。
// 字段见 docs/superpowers/specs/asr-protocol-notes.md 与本设计 §2.1。
type fileASRQueryResponse struct {
	Status   string `json:"status"`
	Error    *struct {
		Stage   string `json:"stage"`
		Message string `json:"message"`
	} `json:"error"`
	Duration float64 `json:"duration"`
	Result   []struct {
		Text       string `json:"text"`
		Utterances []struct {
			Text      string `json:"text"`
			StartTime int    `json:"start_time"` // ms
			EndTime   int    `json:"end_time"`   // ms
			Speaker   *struct {
				ID string `json:"id"` // spk_1..
			} `json:"speaker"`
		} `json:"utterances"`
	} `json:"result"`
}

// ParseFileASRResult 把 StepFun 异步文件 ASR 的 /file/query 响应解析成转写片段（纯函数，可单测）。
// speaker.id 形如 "spk_1" 去前缀得 "1"；speaker 缺失→空 label；start/end_time(ms)→StartMS/EndMS。
func ParseFileASRResult(raw []byte) []TranscriptPiece {
	var resp fileASRQueryResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil
	}
	var out []TranscriptPiece
	for _, r := range resp.Result {
		for _, u := range r.Utterances {
			p := TranscriptPiece{
				Text: u.Text, StartMS: int64(u.StartTime), EndMS: int64(u.EndTime),
			}
			if u.Speaker != nil {
				p.SpeakerLabel = strings.TrimPrefix(u.Speaker.ID, "spk_")
			}
			if p.Text != "" {
				out = append(out, p)
			}
		}
	}
	return out
}
```

（文件顶部 `import` 已有 `encoding/json` 与 `strings`，无需新增。）

- [ ] **Step 4: 跑测试验证通过**

Run: `go test ./internal/provider/ -run TestParseFileASR -v`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/provider/asr.go internal/provider/asr_test.go
git commit -m "feat(provider): parseFileASRResult 解析 StepFun 文件 ASR 响应"
```

### Task 6：StepFunFileASR Provider

**Files:**
- Modify: `internal/provider/asr.go`
- Modify: `internal/provider/asr_test.go`

- [ ] **Step 1: 写失败测试（mock HTTP）**

`internal/provider/asr_test.go` 加：

```go
func TestStepFunFileASRTranscribe(t *testing.T) {
	// 用 httptest 模拟 submit + query；TOS 用假 uploader 返回固定 URL。
	tosUp := &stubTOS{url: "https://example.com/x.wav"}
	var submitCount, queryCount int
	mux := http.NewServeMux()
	mux.HandleFunc("/audio/asr/file/submit", func(w http.ResponseWriter, r *http.Request) {
		submitCount++
		write(w, `{"task_id":"t-1"}`)
	})
	mux.HandleFunc("/audio/asr/file/query", func(w http.ResponseWriter, r *http.Request) {
		queryCount++
		// 第一次 RUNNING，第二次完成
		if queryCount == 1 {
			write(w, `{"status":"RUNNING"}`); return
		}
		write(w, `{"status":"SUCCEEDED","duration":6.2,"result":[{"text":"你好","utterances":[
			{"text":"你好","start_time":2000,"end_time":4500,"speaker":{"id":"spk_1"}}]}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := NewStepFunFileASR(srv.URL, "k", "stepaudio-2.5-asr", tosUp, func(time.Duration) {})
	p.pollInterval = 1 * time.Millisecond
	pieces, err := p.Transcribe(context.Background(), "testdata/speech.wav")
	if err != nil { t.Fatalf("transcribe: %v", err) }
	if submitCount != 1 || queryCount != 2 { t.Fatalf("calls submit=%d query=%d", submitCount, queryCount) }
	if len(pieces) != 1 || pieces[0].SpeakerLabel != "1" || pieces[0].StartMS != 2000 {
		t.Fatalf("%+v", pieces)
	}
	if !tosUp.deleted { t.Fatal("未删 TOS 对象") }
}

type stubTOS struct {
	url     string
	deleted bool
}

func (s *stubTOS) UploadWAV(ctx context.Context, localPath, key string) (string, error) {
	return s.url, nil
}
func (s *stubTOS) Delete(ctx context.Context, key string) error { s.deleted = true; return nil }

func write(w http.ResponseWriter, s string) { w.Header().Set("Content-Type", "application/json"); w.Write([]byte(s)) }
```

> 需在 asr_test.go import `net/http`、`net/http/httptest`、`time`。

- [ ] **Step 2: 跑测试验证失败**

Run: `go test ./internal/provider/ -run TestStepFunFileASR -v`
Expected: FAIL（`StepFunFileASR`/`NewStepFunFileASR` 未定义）。

- [ ] **Step 3: 写实现**

`internal/provider/asr.go` 加（顶部 `import` 增 `"bytes"`、`"io"`、`"net/http"`、`"time"`；如已含则跳过）：

```go
// TOSUploader 抽象 TOS 上传/删除，便于 ASR provider 解耦存储 + 测试注入。
type TOSUploader interface {
	UploadWAV(ctx context.Context, localPath, key string) (presignedURL string, err error)
	Delete(ctx context.Context, key string) error
}

// StepFunFileASR 用 StepFun 异步文件 ASR（/audio/asr/file/submit + /file/query）做转写。
// 原生返回每句 ms 级 start/end_time + speaker.id(spk_N)，见 asr-protocol-notes.md。
type StepFunFileASR struct {
	BaseURL      string // https://api.stepfun.com/v1
	APIKey       string
	Model        string          // stepaudio-2.5-asr
	TOS          TOSUploader
	KeyPrefix    string         // zhiwei/
	sleep        func(time.Duration) // 可注入跳过真实 sleep
	pollInterval time.Duration
}

// NewStepFunFileASR 构造；sleepPtr 用于测试注入假 sleep。
func NewStepFunFileASR(baseURL, apiKey, model string, tos TOSUploader, sleep func(time.Duration)) *StepFunFileASR {
	if sleep == nil {
		sleep = time.Sleep
	}
	return &StepFunFileASR{
		BaseURL: baseURL, APIKey: apiKey, Model: model, TOS: tos,
		KeyPrefix: "zhiwei/", sleep: sleep, pollInterval: 2 * time.Second,
	}
}

func (p *StepFunFileASR) Transcribe(ctx context.Context, audioPath string) ([]TranscriptPiece, error) {
	if p.APIKey == "" {
		return nil, fmt.Errorf("asr: STEPFUN_API_KEY 未设置")
	}
	// 1. 上传 TOS 拿 presigned URL（key 带纳秒避免碰撞；time.Now 可注入不受 workflow 限制）
	key := p.KeyPrefix + fmt.Sprintf("%d.wav", time.Now().UnixNano())
	url, err := p.TOS.UploadWAV(ctx, audioPath, key)
	if err != nil {
		return nil, fmt.Errorf("tos 上传: %w", err)
	}
	defer func() { _ = p.TOS.Delete(ctx, key) }() // best-effort

	// 2. submit
	taskID, err := p.submit(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("asr submit: %w", err)
	}
	// 3. poll query
	raw, err := p.poll(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("asr query: %w", err)
	}
	return ParseFileASRResult(raw), nil
}

func (p *StepFunFileASR) submit(ctx context.Context, audioURL string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"audio":   map[string]any{"format": "wav", "channel": 1, "rate": 16000, "url": audioURL},
		"request": map[string]any{"model_name": p.Model, "show_utterances": true, "enable_speaker_info": true},
	})
	resp, err := p.do(ctx, "POST", p.BaseURL+"/audio/asr/file/submit", body)
	if err != nil {
		return "", err
	}
	var r struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(resp, &r); err != nil || r.TaskID == "" {
		return "", fmt.Errorf("submit 响应非法: %s", resp)
	}
	return r.TaskID, nil
}

func (p *StepFunFileASR) poll(ctx context.Context, taskID string) ([]byte, error) {
	maxAttempts := 150 // 2s × 150 ≈ 5min
	for i := 0; i < maxAttempts; i++ {
		if i > 0 {
			p.sleep(p.pollInterval)
		}
		body, _ := json.Marshal(map[string]string{"task_id": taskID})
		raw, err := p.do(ctx, "POST", p.BaseURL+"/audio/asr/file/query", body)
		if err != nil {
			continue
		}
		var r fileASRQueryResponse
		if err := json.Unmarshal(raw, &r); err != nil {
			continue
		}
		if r.Status == "FAILED" {
			return nil, fmt.Errorf("asr failed: %s", raw)
		}
		if r.Status != "PENDING" && r.Status != "RUNNING" {
			return raw, nil // SUCCEEDED 等
		}
	}
	return nil, fmt.Errorf("asr 超时（task=%s）", taskID)
}

func (p *StepFunFileASR) do(ctx context.Context, method, url string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}
```

- [ ] **Step 4: 跑测试验证通过**

Run: `go test ./internal/provider/ -run TestStepFunFileASR -v`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/provider/asr.go internal/provider/asr_test.go
git commit -m "feat(provider): StepFunFileASR 异步文件 ASR provider"
```

### Task 7：TOS 客户端

**Files:**
- Create: `internal/storage/tos.go`
- Create: `internal/storage/tos_test.go`

- [ ] **Step 1: 写实现（字段名以 Task 1 spike 确认为准）**

`internal/storage/tos.go`：

```go
// Package storage 封装火山引擎 TOS 对象存储：上传私有 wav + 生成 presigned GET URL + 删除。
// 复用 xy/web/tools/tos-upload.mjs 同账号/桶（user-growth/cn-shanghai），key 前缀 zhiwei/。
// 与 ASR provider 解耦：TOSClient 实现 provider.TOSUploader 接口。
package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/volcengine/ve-tos-golang-sdk/v2/tos"
)

// TOSConfig TOS 连接配置。凭证从环境变量 TOS_ACCESS_KEY/TOS_SECRET_KEY 读（main 注入）。
type TOSConfig struct {
	AccessKey string
	SecretKey string
	Region    string // cn-shanghai
	Bucket    string // user-growth
	Endpoint  string // tos-cn-shanghai.volces.com
	KeyPrefix string // zhiwei/
}

type TOSClient struct {
	cfg    TOSConfig
	client *tos.ClientV2
}

func NewTOSClient(cfg TOSConfig) (*TOSClient, error) {
	c, err := tos.NewClientV2(cfg.Endpoint, tos.WithRegion(cfg.Region),
		tos.WithCredentialsProvider(tos.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")))
	if err != nil {
		return nil, err
	}
	return &TOSClient{cfg: cfg, client: c}, nil
}

// UploadWAV 上传本地 wav（私有 ACL），返回 presigned GET URL（1h）。
// key 若不带前缀会自动加 KeyPrefix。
func (t *TOSClient) UploadWAV(ctx context.Context, localPath, key string) (string, error) {
	if !startsWithPrefix(key, t.cfg.KeyPrefix) {
		key = t.cfg.KeyPrefix + key
	}
	_, err := t.client.PutObjectFromFile(ctx, &tos.PutObjectFromFileInput{
		Bucket: t.cfg.Bucket, Key: key, FilePath: localPath, ContentType: "audio/wav",
	})
	if err != nil {
		return "", fmt.Errorf("tos put: %w", err)
	}
	out, err := t.client.PreSignedURL(ctx, &tos.PreSignedURLInput{
		Bucket: t.cfg.Bucket, Key: key, HTTPMethod: tos.HTTPMethodGet, Expires: 3600,
	})
	if err != nil {
		return "", fmt.Errorf("tos presign: %w", err)
	}
	return out.SignedUrl, nil
}

func (t *TOSClient) Delete(ctx context.Context, key string) error {
	if !startsWithPrefix(key, t.cfg.KeyPrefix) {
		key = t.cfg.KeyPrefix + key
	}
	_, err := t.client.DeleteObjectV2(ctx, &tos.DeleteObjectV2Input{Bucket: t.cfg.Bucket, Key: key})
	return err
}

func startsWithPrefix(key, prefix string) bool {
	return len(key) > len(prefix) && key[:len(prefix)] == prefix
}

// 兼容编译期引用 time（presign TTL 扩展点）
var _ = time.Second
```

> 若 Task 1 spike 发现字段名不同（如 `PreSignedURL` 无 `Expires` 字段，改为 `tos.NewSignedURL...` 等），按 spike 注释修正此处。

- [ ] **Step 2: 跑编译 + spike 集成验证**

Run: `go build ./internal/storage/`
Run（真实）：`TOS_ACCESS_KEY=.. TOS_SECRET_KEY=.. go run ./cmd/spike/tos testdata/speech.wav`
Expected: 编译通过；spike 跑通（见 Task 1）。

- [ ] **Step 3: 写单元测试（仅编译期 + 接口符合性）**

`internal/storage/tos_test.go`：

```go
package storage

import "testing"

// TOSClient 真实调用依赖云凭证，不进单测；仅校验构造与接口符合性。
func TestTOSClientImplementsUploader(t *testing.T) {
	var _ interface {
		UploadWAV(ctx context.Context, localPath, key string) (string, error)
		Delete(ctx context.Context, key string) error
	} = (*TOSClient)(nil)
}

func TestStartsWithPrefix(t *testing.T) {
	if !startsWithPrefix("zhiwei/a.wav", "zhiwei/") { t.Fatal() }
	if startsWithPrefix("zhiwei", "zhiwei/") { t.Fatal() }
}
```
（需 import `context`。）

- [ ] **Step 4: 跑测试 + 提交**

Run: `go test ./internal/storage/ -v`
Expected: PASS。

```bash
git add internal/storage/
git commit -m "feat(storage): TOS 客户端 上传/presign/删除"
```

### Task 8：ASR stage 接入文件 ASR + TOS

**Files:**
- Modify: `internal/pipeline/stage_asr.go`
- Modify: `internal/pipeline/stage_asr_test.go`

- [ ] **Step 1: 调整 stageASR 依赖**

`internal/pipeline/stage_asr.go` 中 `StageDeps` 已有 `ASR provider.ASRProvider`、`DataDir`。文件 ASR 需 TOS，但 TOS 已注入到 `StepFunFileASR` 内部（main 装配时），stage 不直接依赖 TOS。**无需改 stageASR 逻辑**——它仍调 `d.ASR.Transcribe(ctx, wavPath)`，provider 内部自处理 TOS。仅需确保 stage 测试用 fake ASR 即可。

验证现有 stage_asr_test 仍通过：

Run: `go test ./internal/pipeline/ -run stageASR -v`
Expected: PASS（provider 注入的是 mock，TOS 在 provider 内）。

- [ ] **Step 2: 提交（如有改动）**

```bash
git add internal/pipeline/stage_asr.go internal/pipeline/stage_asr_test.go
git commit -m "chore(pipeline): asr stage 由 provider 内部处理 TOS，逻辑不变" --allow-empty
```

> 若无改动跳过空提交。

---

## Phase 3：Python sidecar

### Task 9：voiceprint sidecar

**Files:**
- Create: `services/voiceprint/embedder.py`
- Create: `services/voiceprint/app.py`
- Create: `services/voiceprint/test_app.py`
- Modify: `services/voiceprint/requirements.txt`

- [ ] **Step 1: 写 embedder 接口 + WeSpeaker 实现**

`services/voiceprint/embedder.py`：

```python
"""声纹提取抽象 + WeSpeaker 实现。
Embedder.embed(wav_path) -> np.ndarray(256,) 已 L2 归一。
WeSpeaker 加载逻辑由 spike（spike.py）确认后填充 _WeSpeakerEmbedder.load_embedder。
"""
from __future__ import annotations
import numpy as np


class Embedder:
    def embed(self, wav_path: str) -> np.ndarray:
        raise NotImplementedError


class StubEmbedder(Embedder):
    """测试用：对同一路径返回稳定 256 维向量。"""
    def __init__(self) -> None:
        self._cache: dict[str, np.ndarray] = {}

    def embed(self, wav_path: str) -> np.ndarray:
        if wav_path not in self._cache:
            # 路径哈希 → 稳定向量，便于 /add 后 /search 命中
            seed = abs(hash(wav_path)) % 1000
            rng = np.random.default_rng(seed)
            v = rng.standard_normal(256).astype(np.float32)
            v /= (np.linalg.norm(v) + 1e-12)
            self._cache[wav_path] = v
        return self._cache[wav_path]


def load_embedder() -> Embedder:
    """生产：返回 WeSpeaker resnet34-LM embedder。
    由 spike.py 验证加载方式后在此实现：
      1. 下载/定位 resnet34-LM 权重（87MB）；
      2. ONNX Runtime 或 wespeaker python 包加载；
      3. embed(wav_path) 提取 256 维并 L2 归一。
    未就绪前返回 StubEmbedder（仅用于 sidecar 自测/联调，不进生产）。
    """
    try:
        return _WeSpeakerEmbedder()
    except Exception:
        return StubEmbedder()


class _WeSpeakerEmbedder(Embedder):
    def __init__(self) -> None:
        # TODO(spike→固化): 见 spike.py，填充真实加载
        raise NotImplementedError("WeSpeaker 加载未实现，见 spike.py")

    def embed(self, wav_path: str) -> np.ndarray:
        raise NotImplementedError
```

- [ ] **Step 2: 写 FastAPI app**

`services/voiceprint/app.py`：

```python
"""声纹 sidecar：WeSpeaker 提向量 + FAISS 1:N。
契约见 spec §6.1：
  POST /embed   {audio_path}          -> {vector:[256 float]}
  POST /search  {vector}              -> {speaker_id, distance} | {matched:false}
  POST /add     {vector, speaker_id}  -> {ok:true}     幂等：先 remove 再 add
  POST /remove  {speaker_id}          -> {ok:true}
  GET  /health                          -> {status, model, n_vectors}
索引落盘 data/voiceprint.index（IndexIDMap2(IndexFlatIP(256))）。
"""
from __future__ import annotations
import os
import threading
import numpy as np
from fastapi import FastAPI
from pydantic import BaseModel
from .embedder import load_embedder, StubEmbedder

try:
    import faiss
except ImportError:  # CI 无 faiss 时用 numpy 暴力实现
    faiss = None

INDEX_PATH = os.environ.get("ZW_VOICEPRINT_INDEX", "data/voiceprint.index")
DIM = 256

app = FastAPI()
_embedder = load_embedder()
_lock = threading.Lock()
_index = None  # faiss.IndexIDMap2 | _NumpyIndex
_id2count = 0


class VecReq(BaseModel):
    vector: list[float]


class AddReq(BaseModel):
    vector: list[float]
    speaker_id: int


class RemoveReq(BaseModel):
    speaker_id: int


class EmbedReq(BaseModel):
    audio_path: str


def _load_index():
    global _index
    if faiss is not None:
        if os.path.exists(INDEX_PATH):
            _index = faiss.read_index(INDEX_PATH)
        else:
            base = faiss.IndexFlatIP(DIM)
            _index = faiss.IndexIDMap2(base)
    else:
        _index = _NumpyIndex()


class _NumpyIndex:
    """faiss 不可用时的纯 numpy 暴力余弦索引（CI 用）。"""
    def __init__(self) -> None:
        self.vecs: list[np.ndarray] = []
        self.ids: list[int] = []

    def search(self, q: np.ndarray):
        if not self.vecs:
            return None
        M = np.stack(self.vecs)
        sims = M @ q
        i = int(np.argmax(sims))
        return self.ids[i], float(sims[i])

    def remove(self, sid: int) -> None:
        self.vecs = [v for v, i in zip(self.vecs, self.ids) if i != sid]
        self.ids = [i for i in self.ids if i != sid]

    def add(self, v: np.ndarray, sid: int) -> None:
        self.remove(sid)
        self.vecs.append(v)
        self.ids.append(sid)

    def count(self) -> int:
        return len(self.ids)

    def save(self) -> None:
        np.save(INDEX_PATH + ".npz",
                {"vecs": np.stack(self.vecs) if self.vecs else np.zeros((0, DIM), dtype=np.float32),
                 "ids": np.array(self.ids, dtype=np.int64)}) if self.vecs else None

    def load(self) -> None:
        if os.path.exists(INDEX_PATH + ".npz"):
            d = np.load(INDEX_PATH + ".npz")
            self.vecs = list(d["vecs"].astype(np.float32))
            self.ids = list(d["ids"].tolist())


@app.on_event("startup")
def _startup() -> None:
    _load_index()
    if isinstance(_index, _NumpyIndex):
        _index.load()


def _to_vec(arr) -> np.ndarray:
    v = np.asarray(arr, dtype=np.float32)
    if v.shape != (DIM,):
        raise ValueError(f"期望 {DIM} 维，实际 {v.shape}")
    n = np.linalg.norm(v)
    if n > 0:
        v = v / n
    return v


@app.get("/health")
def health() -> dict:
    return {"status": "ok",
            "model": type(_embedder).__name__,
            "n_vectors": _index.count() if isinstance(_index, _NumpyIndex) else _index.ntotal}


@app.post("/embed")
def embed(req: EmbedReq) -> dict:
    v = _embedder.embed(req.audio_path)
    v = v / (np.linalg.norm(v) + 1e-12)
    return {"vector": v.tolist()}


@app.post("/search")
def search(req: VecReq) -> dict:
    q = _to_vec(req.vector)
    res = _index.search(q) if isinstance(_index, _NumpyIndex) else _search_faiss(q)
    if res is None:
        return {"matched": False}
    sid, dist = res
    return {"speaker_id": sid, "distance": dist, "matched": True}


def _search_faiss(q: np.ndarray):
    q2 = q.reshape(1, -1)
    D, I = _index.search(q2, 1)
    if I[0][0] == -1:
        return None
    return int(I[0][0]), float(D[0][0])


@app.post("/add")
def add(req: AddReq) -> dict:
    v = _to_vec(req.vector)
    with _lock:
        if isinstance(_index, _NumpyIndex):
            _index.add(v, req.speaker_id)
            _index.save()
        else:
            _index.remove_ids(np.array([req.speaker_id], dtype=np.int64))
            _index.add_with_ids(v.reshape(1, -1), np.array([req.speaker_id], dtype=np.int64))
            faiss.write_index(_index, INDEX_PATH)
    return {"ok": True}


@app.post("/remove")
def remove(req: RemoveReq) -> dict:
    with _lock:
        if isinstance(_index, _NumpyIndex):
            _index.remove(req.speaker_id)
            _index.save()
        else:
            _index.remove_ids(np.array([req.speaker_id], dtype=np.int64))
            faiss.write_index(_index, INDEX_PATH)
    return {"ok": True}
```

- [ ] **Step 3: 写 pytest（stub embedder + numpy 索引，无需 faiss）**

`services/voiceprint/test_app.py`：

```python
import os
import tempfile
from fastapi.testclient import TestClient

os.environ["ZW_VOICEPRINT_INDEX"] = tempfile.mktemp(suffix=".npz")

# 强制 numpy 索引（撤掉 faiss）
import sys, types
sys.modules["faiss"] = None  # type: ignore

from services.voiceprint import app as appmod  # noqa: E402
appmod.faiss = None
appmod._index = appmod._NumpyIndex()
appmod._embedder = appmod.StubEmbedder()

client = TestClient(app.app)


def test_embed_dim():
    with tempfile.NamedTemporaryFile(suffix=".wav") as f:
        r = client.post("/embed", json={"audio_path": f.name})
        assert r.status_code == 200
        v = r.json()["vector"]
        assert len(v) == 256


def test_add_search_remove():
    with tempfile.NamedTemporaryFile(suffix=".wav") as f:
        name = f.name
    v = client.post("/embed", json={"audio_path": name}).json()["vector"]
    assert client.post("/search", json={"vector": v}).json()["matched"] is False  # 空
    assert client.post("/add", json={"vector": v, "speaker_id": 42}).json()["ok"] is True
    r = client.post("/search", json={"vector": v}).json()
    assert r["matched"] is True and r["speaker_id"] == 42
    assert client.post("/remove", json={"speaker_id": 42}).json()["ok"] is True
    assert client.post("/search", json={"vector": v}).json()["matched"] is False


def test_health():
    r = client.get("/health")
    assert r.status_code == 200 and r.json()["status"] == "ok"
```

- [ ] **Step 4: 跑 pytest**

Run: `cd services/voiceprint && python -m pytest -v` （需先 `pip install fastapi httpx pytest`）
Expected: 3 PASS。

- [ ] **Step 5: 提交**

```bash
git add services/voiceprint/
git commit -m "feat(voiceprint): Python sidecar WeSpeaker+FAISS /embed /search /add /remove"
```

---

## Phase 4：voiceprint 客户端 + speaker stage

### Task 10：voiceprint Go 客户端

**Files:**
- Create: `internal/voiceprint/client.go`
- Create: `internal/voiceprint/client_test.go`

- [ ] **Step 1: 写失败测试（mock HTTP）**

`internal/voiceprint/client_test.go`：

```go
package voiceprint

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"zhiwei/internal/ids"
)

func TestClientSearchMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/search" {
			_, _ = w.Write([]byte(`{"speaker_id":42,"distance":0.81,"matched":true}`))
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL)
	id, dist, matched, err := c.Search(context.Background(), make([]float32, 256))
	if err != nil { t.Fatal(err) }
	if !matched || id.Int64() != 42 || dist != 0.81 { t.Fatalf("%v %d %f", matched, id, dist) }
}

func TestClientSearchEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"matched":false}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL)
	_, _, matched, err := c.Search(context.Background(), make([]float32, 256))
	if err != nil || matched { t.Fatalf("%v %v", matched, err) }
}

func TestClientAddRemove(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL)
	if err := c.Add(context.Background(), make([]float32, 256), ids.New()); err != nil { t.Fatal(err) }
	if gotPath != "/add" { t.Fatalf(gotPath) }
	if err := c.Remove(context.Background(), ids.ID(42)); err != nil { t.Fatal(err) }
	if gotPath != "/remove" { t.Fatalf(gotPath) }
}

func TestClientEmbed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vec := make([]float32, 256)
		b, _ := json.Marshal(map[string]any{"vector": vec})
		_, _ = w.Write(b)
	}))
	defer srv.Close()
	c := NewClient(srv.URL)
	v, err := c.Embed(context.Background(), "x.wav")
	if err != nil || len(v) != 256 { t.Fatalf("%v %d", err, len(v)) }
}
```

- [ ] **Step 2: 跑测试验证失败**

Run: `go test ./internal/voiceprint/ -v`
Expected: FAIL（包未定义）。

- [ ] **Step 3: 写实现**

`internal/voiceprint/client.go`：

```go
// Package voiceprint 封装声纹 sidecar 的 HTTP 调用。
// sidecar 契约见 docs/superpowers/specs/2026-08-22-speaker-voiceprint-design.md §6.1。
package voiceprint

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"zhiwei/internal/ids"
)

// Client 声纹 sidecar 客户端接口（pipeline/api 注入，测试可 mock）。
type Client interface {
	Embed(ctx context.Context, audioPath string) ([]float32, error)
	Search(ctx context.Context, vec []float32) (speakerID ids.ID, distance float64, matched bool, err error)
	Add(ctx context.Context, vec []float32, id ids.ID) error
	Remove(ctx context.Context, id ids.ID) error
}

type httpClient struct {
	BaseURL string
	hc      *http.Client
}

func NewClient(baseURL string) Client {
	return &httpClient{BaseURL: baseURL, hc: http.DefaultClient}
}

func (c *httpClient) post(ctx context.Context, path string, body any, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("voiceprint %s: http %d: %s", path, resp.StatusCode, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("voiceprint %s 解析: %w", path, err)
		}
	}
	return nil
}

func (c *httpClient) Embed(ctx context.Context, audioPath string) ([]float32, error) {
	var out struct {
		Vector []float32 `json:"vector"`
	}
	if err := c.post(ctx, "/embed", map[string]string{"audio_path": audioPath}, &out); err != nil {
		return nil, err
	}
	return out.Vector, nil
}

func (c *httpClient) Search(ctx context.Context, vec []float32) (ids.ID, float64, bool, error) {
	var out struct {
		SpeakerID int64   `json:"speaker_id"`
		Distance   float64 `json:"distance"`
		Matched    bool    `json:"matched"`
	}
	if err := c.post(ctx, "/search", map[string][]float32{"vector": vec}, &out); err != nil {
		return 0, 0, false, err
	}
	return ids.ID(out.SpeakerID), out.Distance, out.Matched, nil
}

func (c *httpClient) Add(ctx context.Context, vec []float32, id ids.ID) error {
	return c.post(ctx, "/add", map[string]any{"vector": vec, "speaker_id": id.Int64()}, nil)
}

func (c *httpClient) Remove(ctx context.Context, id ids.ID) error {
	return c.post(ctx, "/remove", map[string]int64{"speaker_id": id.Int64()}, nil)
}
```

- [ ] **Step 4: 跑测试验证通过**

Run: `go test ./internal/voiceprint/ -v`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/voiceprint/
git commit -m "feat(voiceprint): sidecar HTTP 客户端"
```

### Task 11：speaker stage

**Files:**
- Create: `internal/pipeline/stage_speaker.go`
- Create: `internal/pipeline/stage_speaker_test.go`
- Modify: `internal/repo/transcript.go`（增方法）
- Modify: `internal/repo/transcript_test.go`（增测试）

- [ ] **Step 1: 给 TranscriptRepo 加回填方法（先测试）**

`internal/repo/transcript_test.go` 加：

```go
func TestSetSegmentSpeaker(t *testing.T) {
	if testing.Short() { t.Skip("需要 MySQL") }
	ctx := context.Background()
	tr := seedTranscript(ctx, t) // 见下方 helper；返回已插 transcript + 一段 seg(label="1")
	segs, _ := testTranscripts.ListSegments(ctx, tr.ID)
	segID := segs[0].ID
	sid := ids.New()
	if err := testTranscripts.SetSegmentSpeaker(ctx, tr.ID, "1", sid); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, _ := testTranscripts.ListSegments(ctx, tr.ID)
	if got[0].SpeakerID == nil || *got[0].SpeakerID != sid {
		t.Fatalf("speaker_id 未回填: %+v", got[0])
	}
	_ = segID
}

func TestSetSegmentSpeakerByID(t *testing.T) {
	if testing.Short() { t.Skip("需要 MySQL") }
	ctx := context.Background()
	tr := seedTranscript(ctx, t)
	segs, _ := testTranscripts.ListSegments(ctx, tr.ID)
	sid := ids.New()
	if err := testTranscripts.SetSegmentSpeakerByID(ctx, tr.ID, segs[0].ID, sid); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, _ := testTranscripts.ListSegments(ctx, tr.ID)
	if got[0].SpeakerID == nil || *got[0].SpeakerID != sid { t.Fatalf("未回填") }
}
```

> `seedTranscript` helper：插一条 transcript + 一段 speaker_label="1" 的 segment，复用现有 `TranscriptRepo.Create`/`InsertSegments`。若 transcript_test.go 已有类似 helper 则复用。

- [ ] **Step 2: 跑测试验证失败**

Run: `TEST_MYSQL_DSN="..." go test ./internal/repo/ -run TestSetSegmentSpeaker -v`
Expected: FAIL（方法未定义）。

- [ ] **Step 3: 实现 TranscriptRepo 方法**

`internal/repo/transcript.go` 中 `TranscriptSegment` 结构加字段：

```go
type TranscriptSegment struct {
	ID           ids.ID    `db:"id" json:"id"`
	TranscriptID ids.ID    `db:"transcript_id" json:"transcript_id"`
	SequenceNo   int       `db:"sequence_no" json:"sequence_no"`
	SpeakerLabel string    `db:"speaker_label" json:"speaker_label"`
	SpeakerID    *ids.ID   `db:"speaker_id" json:"speaker_id,omitempty"` // 新增：解析到的已登记说话人
	Text         string    `db:"text" json:"text"`
	StartMS      int64     `db:"start_ms" json:"start_ms"`
	EndMS        int64     `db:"end_ms" json:"end_ms"`
	Confidence   *float64  `db:"confidence" json:"confidence"`
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
}
```

并加方法：

```go
// SetSegmentSpeaker 按 speaker_label 批量回填本 transcript 内段的 speaker_id。
// 带 transcript_id 作用域防跨会话；原子 UPDATE。rows=0 静默。
func (r *TranscriptRepo) SetSegmentSpeaker(ctx context.Context, transcriptID ids.ID, speakerLabel string, speakerID ids.ID) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE transcript_segment SET speaker_id = ? WHERE transcript_id = ? AND speaker_label = ?`,
		speakerID.Int64(), transcriptID.Int64(), speakerLabel)
	return err
}

// SetSegmentSpeakerByID 单段换人（前端"换人"用）。带 transcript_id 作用域防跨会话。
func (r *TranscriptRepo) SetSegmentSpeakerByID(ctx context.Context, transcriptID, segID, speakerID ids.ID) error {
	_, err := r.DB.ExecContext(ctx,
		`UPDATE transcript_segment SET speaker_id = ? WHERE id = ? AND transcript_id = ?`,
		speakerID.Int64(), segID.Int64(), transcriptID.Int64())
	return err
}

// ListSpeakersForTranscript 本 transcript 解析到的说话人聚合（panel 用）。
func (r *TranscriptRepo) ListSpeakersForTranscript(ctx context.Context, transcriptID ids.ID) ([]SpeakerInSegment, error) {
	var list []SpeakerInSegment
	err := r.DB.SelectContext(ctx, &list, `
SELECT s.id AS speaker_id, s.name, s.source, COUNT(seg.id) AS segment_count
FROM transcript_segment seg
JOIN speaker s ON s.id = seg.speaker_id
WHERE seg.transcript_id = ? AND seg.speaker_id IS NOT NULL
GROUP BY s.id, s.name, s.source
ORDER BY MIN(seg.sequence_no)`, transcriptID.Int64())
	return list, err
}

type SpeakerInSegment struct {
	SpeakerID    ids.ID `db:"speaker_id" json:"speaker_id"`
	Name         string `db:"name" json:"name"`
	Source       string `db:"source" json:"source"`
	SegmentCount int    `db:"segment_count" json:"segment_count"`
}
```

- [ ] **Step 4: 跑 repo 测试通过**

Run: `TEST_MYSQL_DSN="..." go test ./internal/repo/ -run TestSetSegmentSpeaker -v`
Expected: PASS。

- [ ] **Step 5: 写 speaker stage 失败测试（fake voiceprint + fake repo）**

`internal/pipeline/stage_speaker_test.go`：

```go
package pipeline

import (
	"context"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/provider"
	"zhiwei/internal/repo"
)

// fakeVoiceprint 记录调用，可控 matched。
type fakeVoiceprint struct {
	embedSeq int
	matched  bool
	matchID  ids.ID
	added     []ids.ID
	searched  int
}

func (f *fakeVoiceprint) Embed(ctx context.Context, p string) ([]float32, error) {
	f.embedSeq++
	return make([]float32, 256), nil // 每段返回零向量（聚合后仍零）
}
func (f *fakeVoiceprint) Search(ctx context.Context, v []float32) (ids.ID, float64, bool, error) {
	f.searched++
	return f.matchID, 0.9, f.matched, nil
}
func (f *fakeVoiceprint) Add(ctx context.Context, v []float32, id ids.ID) error { f.added = append(f.added, id); return nil }
func (f *fakeVoiceprint) Remove(ctx context.Context, id ids.ID) error            { return nil }

func TestStageSpeakerEnrollsWhenNoMatch(t *testing.T) {
	// 需 MySQL（建 transcript+segments）。用 testDB。
	if testing.Short() { t.Skip("需要 MySQL") }
	ctx := context.Background()
	tr := &repo.Transcript{SessionID: ids.New(), Language: "zh-CN"}
	_ = testTranscripts.Create(ctx, tr)
	_ = testTranscripts.InsertSegments(ctx, []repo.TranscriptSegment{
		{TranscriptID: tr.ID, SequenceNo: 1, SpeakerLabel: "1", Text: "a", StartMS: 0, EndMS: 1000},
		{TranscriptID: tr.ID, SequenceNo: 2, SpeakerLabel: "1", Text: "b", StartMS: 1000, EndMS: 2000},
	})

	fv := &fakeVoiceprint{matched: false}
	d := StageDeps{Transcripts: testTranscripts, Speakers: testSpeakers, Voiceprint: fv, DataDir: t.TempDir()}
	err := runSpeakerStage(ctx, d, ids.New(), tr)
	if err != nil { t.Fatalf("stage: %v", err) }
	if len(fv.added) != 1 { t.Fatalf("应登记 1 个，实际 %d", len(fv.added)) }
	segs, _ := testTranscripts.ListSegments(ctx, tr.ID)
	for _, s := range segs {
		if s.SpeakerID == nil || *s.SpeakerID != fv.added[0] { t.Fatalf("段未回填: %+v", s) }
	}
}

func TestStageSpeakerMatchesExisting(t *testing.T) {
	if testing.Short() { t.Skip("需要 MySQL") }
	ctx := context.Background()
	// 预置一个已登记 speaker
	sp := &repo.Speaker{Name: "张三", Source: "enrolled"}
	_ = testSpeakers.Create(ctx, sp)
	tr := &repo.Transcript{SessionID: ids.New(), Language: "zh-CN"}
	_ = testTranscripts.Create(ctx, tr)
	_ = testTranscripts.InsertSegments(ctx, []repo.TranscriptSegment{
		{TranscriptID: tr.ID, SequenceNo: 1, SpeakerLabel: "1", Text: "a", StartMS: 0, EndMS: 1000},
	})

	fv := &fakeVoiceprint{matched: true, matchID: sp.ID}
	d := StageDeps{Transcripts: testTranscripts, Speakers: testSpeakers, Voiceprint: fv, DataDir: t.TempDir()}
	if err := runSpeakerStage(ctx, d, ids.New(), tr); err != nil { t.Fatalf("stage: %v", err) }
	if len(fv.added) != 0 { t.Fatalf("命中时不应登记，实际 %d", len(fv.added)) }
	segs, _ := testTranscripts.ListSegments(ctx, tr.ID)
	if segs[0].SpeakerID == nil || *segs[0].SpeakerID != sp.ID { t.Fatalf("未回填命中: %+v", segs[0]) }
}

// 用 provider.TranscriptPiece 占位避免未用 import 报错
var _ = provider.TranscriptPiece{}
```

> `testTranscripts`/`testSpeakers`/`testDB` 为 pipeline 包测试共享 fixture（见 `main_test.go`），Task 中补充装配。若不存在则在 main_test.go 增 `testSpeakers = &repo.SpeakerRepo{DB: testDB}`。

- [ ] **Step 6: 跑测试验证失败**

Run: `TEST_MYSQL_DSN="..." go test ./internal/pipeline/ -run TestStageSpeaker -v`
Expected: FAIL（`runSpeakerStage`/`Voiceprint` 字段未定义）。

- [ ] **Step 7: 实现 StageDeps 字段 + stageSpeaker**

`internal/pipeline/stage_asr.go` 中 `StageDeps` 加字段：

```go
	// ---- speaker stage ----
	Voiceprint voiceprint.Client
	Speakers   *repo.SpeakerRepo
	VoiceprintThreshold float64 // ZW_VOICEPRINT_THRESHOLD，0 表示用默认
```

（import 增 `"zhiwei/internal/voiceprint"`。）

`internal/pipeline/stage_speaker.go`：

```go
// stage_speaker 实现 speaker stage：切片→提向→聚合→1:N 检索/登记→回填 segment.speaker_id。
package pipeline

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
	"zhiwei/internal/voiceprint"
)

// BuildStages（stage_asr.go）的 map 增 "speaker": stageSpeaker(d)。
// runSpeakerStage 暴露给测试直接调用（避开 pool）。
func runSpeakerStage(ctx context.Context, d StageDeps, sessionID ids.ID, tr *repo.Transcript) error {
	segs, err := d.Transcripts.ListSegments(ctx, tr.ID)
	if err != nil {
		return fmt.Errorf("读 segments: %w", err)
	}
	// 按 speaker_label 分组（空 label 归 ""）
	groups := map[string][]repo.TranscriptSegment{}
	for _, s := range segs {
		groups[s.SpeakerLabel] = append(groups[s.SpeakerLabel], s)
	}
	sliceDir := filepath.Join(d.DataDir, "slices", sessionID.String())
	_ = os.MkdirAll(sliceDir, 0o755)
	wavPath := filepath.Join(d.DataDir, "transcoded", sessionID.String()+".wav")

	threshold := d.VoiceprintThreshold
	if threshold == 0 {
		threshold = 0.5
	}

	for label, members := range groups {
		// 逐段切片 + 提向
		vecs := make([][]float32, 0, len(members))
		for _, s := range members {
			slicePath := filepath.Join(sliceDir, fmt.Sprintf("seg-%d.wav", s.SequenceNo))
			if err := sliceAudio(wavPath, slicePath, s.StartMS, s.EndMS); err != nil {
				continue // 切片失败跳过该段
			}
			v, err := d.Voiceprint.Embed(ctx, slicePath)
			if err != nil || len(v) != 256 {
				continue // 提向失败跳过
			}
			vecs = append(vecs, v)
		}
		if len(vecs) == 0 {
			continue
		}
		rep := aggregateEmbeddings(vecs)
		// 1:N
		matchID, dist, matched, err := d.Voiceprint.Search(ctx, rep)
		if err != nil {
			return fmt.Errorf("voiceprint search: %w", err)
		}
		matched = matched && dist >= threshold
		var speakerID ids.ID
		if matched {
			speakerID = matchID
		} else {
			// 自动登记
			sp := &repo.Speaker{Name: "说话人" + rand5(), Source: "auto", Embedding: float32Blob(rep), SampleCount: len(vecs)}
			if err := d.Speakers.Create(ctx, sp); err != nil {
				return fmt.Errorf("登记 speaker: %w", err)
			}
			if err := d.Voiceprint.Add(ctx, rep, sp.ID); err != nil {
				return fmt.Errorf("voiceprint add: %w", err)
			}
			speakerID = sp.ID
		}
		// 回填组内段
		if err := d.Transcripts.SetSegmentSpeaker(ctx, tr.ID, label, speakerID); err != nil {
			return fmt.Errorf("回填 speaker_id: %w", err)
		}
	}
	// 清理切片（best-effort）
	_ = os.RemoveAll(sliceDir)
	return nil
}

// stageSpeaker 是 pool 用的 Handler 包装。
func stageSpeaker(d StageDeps) Handler {
	return func(ctx context.Context, _ *repo.Job, sessionID ids.ID) error {
		tr, err := d.Transcripts.GetBySession(ctx, sessionID)
		if err != nil {
			return fmt.Errorf("读 transcript: %w", err)
		}
		return runSpeakerStage(ctx, d, sessionID, tr)
	}
}

// sliceAudio 用 ffmpeg 从 transcoded wav 按毫秒切出片段。PCM 样本级精确（-c copy）。
func sliceAudio(src, dst string, startMS, endMS int64) error {
	args := []string{"-y",
		"-ss", fmt.Sprintf("%.3f", float64(startMS)/1000),
		"-to", fmt.Sprintf("%.3f", float64(endMS)/1000),
		"-i", src, "-c", "copy", dst}
	cmd := exec.Command("ffmpeg", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg slice: %w: %s", err, out)
	}
	return nil
}

// aggregateEmbeddings 多段向量取均值再 L2 归一，得代表声纹。
func aggregateEmbeddings(vecs [][]float32) []float32 {
	rep := make([]float32, 256)
	for _, v := range vecs {
		for i := 0; i < 256; i++ {
			rep[i] += v[i]
		}
	}
	var n float32
	for i := 0; i < 256; i++ {
		rep[i] /= float32(len(vecs))
		n += rep[i] * rep[i]
	}
	if n > 0 {
		s := 1 / float32(sqrtF32(float64(n)))
		for i := 0; i < 256; i++ {
			rep[i] *= s
		}
	}
	return rep
}

func sqrtF32(x float64) float64 { // 避免 import math 仅为此
	// Newton 一次足够（向量归一不要求高精度）
	if x <= 0 {
		return 0
	}
	g := x
	for i := 0; i < 4; i++ {
		g = (g + x/g) / 2
	}
	return g
}

// float32Blob 256×float32 → []byte（存 speaker.embedding BLOB）。
func float32Blob(v []float32) []byte {
	buf := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], mathFloat32bits(x))
	}
	return buf
}

// rand5 生成 5 位 [a-z0-9] 随机串。
func rand5() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	var b [5]byte
	_, _ = rand.Read(b[:])
	out := make([]byte, 5)
	for i := range b {
		out[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(out)
}

// mathFloat32bits 避免此处 import math（math.Float32bits）；内联实现。
func mathFloat32bits(f float32) uint32 {
	return *(*uint32)(asUnsafePointer(&f))
}
```

> `asUnsafePointer` 用 `unsafe.Pointer`；为避免在计划里塞 unsafe 细节出错，**实际实现时直接 `import "math"` 用 `math.Float32bits`** 并删除 `mathFloat32bits`/`sqrtF32`（改用 `math.Sqrt`）。计划此处写明：用 `math.Float32bits` 与 `math.Sqrt`，import `"math"`。即在 Step 8 落地时把上述 helper 替换为标准库实现。

并在 `stage_asr.go` 的 `BuildStages` map 注册：`"speaker": stageSpeaker(d),`。

- [ ] **Step 8: 落地修正 + 跑测试通过**

把 `stage_speaker.go` 顶部的 `mathFloat32bits`/`sqrtF32`/`asUnsafePointer` 删除，改 import `"math"`，`float32Blob` 内用 `math.Float32bits(x)`，`aggregateEmbeddings` 内用 `math.Sqrt`。`asUnsafePointer` 同删。

Run: `go build ./internal/pipeline/ && TEST_MYSQL_DSN="..." go test ./internal/pipeline/ -run TestStageSpeaker -v`
Expected: PASS。

- [ ] **Step 9: 提交**

```bash
git add internal/pipeline/stage_speaker.go internal/pipeline/stage_speaker_test.go internal/pipeline/stage_asr.go internal/repo/transcript.go internal/repo/transcript_test.go
git commit -m "feat(pipeline): speaker stage 切片/提向/聚合/检索/登记/回填"
```

### Task 12：装配 Flow + main.go + pool

**Files:**
- Modify: `internal/pipeline/state.go`（或 Flow 定义处）
- Modify: `cmd/zhiwei-server/main.go`
- Modify: `internal/config/config.go`

- [ ] **Step 1: Flow 增 speaker stage**

定位 `Flow` 使用处（main.go：`flow := pipeline.Flow{Stages: []string{"asr", "segment", "extract"}}`），改为：

```go
flow := pipeline.Flow{Stages: []string{"asr", "segment", "speaker", "extract"}}
```

- [ ] **Step 2: config 增字段**

`internal/config/config.go` 的 `Config` 加：

```go
	// ---- speaker stage ----
	TOSAccessKey string
	TOSSecretKey string
	TOSRegion    string
	TOSBucket    string
	TOSEndpoint  string
	TOSKeyPrefix string

	StepFunASRBase     string // https://api.stepfun.com/v1
	ASRModel           string // stepaudio-2.5-asr（覆盖 ZW_ASR_MODEL）

	VoiceprintSidecarURL string
	VoiceprintThreshold   float64
```

`Load()` 里填：

```go
		TOSAccessKey: os.Getenv("TOS_ACCESS_KEY"),
		TOSSecretKey: os.Getenv("TOS_SECRET_KEY"),
		TOSRegion:    getenv("ZW_TOS_REGION", "cn-shanghai"),
		TOSBucket:    getenv("ZW_TOS_BUCKET", "user-growth"),
		TOSEndpoint:  getenv("ZW_TOS_ENDPOINT", "tos-cn-shanghai.volces.com"),
		TOSKeyPrefix: getenv("ZW_TOS_KEY_PREFIX", "zhiwei/"),

		StepFunASRBase: getenv("ZW_STEPFUN_ASR_BASE", "https://api.stepfun.com/v1"),
		ASRModel:       getenv("ZW_ASR_MODEL", "stepaudio-2.5-asr"),

		VoiceprintSidecarURL: getenv("ZW_VOICEPRINT_SIDECAR_URL", "http://127.0.0.1:8010"),
		VoiceprintThreshold:   getenvFloat("ZW_VOICEPRINT_THRESHOLD", 0.5),
```

`config_test.go` 增默认值断言（可选）。

- [ ] **Step 3: main.go 装配**

`cmd/zhiwei-server/main.go`：在 `StepFunAPIKey` 校验后增 TOS 校验：

```go
	if cfg.TOSAccessKey == "" || cfg.TOSSecretKey == "" {
		log.Fatal("TOS_ACCESS_KEY/TOS_SECRET_KEY 未设置：ASR 文件接口需要 TOS 上传音频")
	}
```

替换 ASR 构造：

```go
	tosClient, err := storage.NewTOSClient(storage.TOSConfig{
		AccessKey: cfg.TOSAccessKey, SecretKey: cfg.TOSSecretKey,
		Region: cfg.TOSRegion, Bucket: cfg.TOSBucket, Endpoint: cfg.TOSEndpoint, KeyPrefix: cfg.TOSKeyPrefix,
	})
	if err != nil {
		log.Fatal("TOS 客户端: ", err)
	}
	asr := provider.NewStepFunFileASR(cfg.StepFunASRBase, cfg.StepFunAPIKey, cfg.ASRModel, tosClient, nil)
	voiceprintCli := voiceprint.NewClient(cfg.VoiceprintSidecarURL)
	speakers := &repo.SpeakerRepo{DB: db}
```

`StageDeps` 注入新增：

```go
	stages := pipeline.BuildStages(pipeline.StageDeps{
		Sessions: sessions, Transcripts: transcripts, ASR: asr, DataDir: cfg.DataDir,
		DB: db, Memories: memories, Todos: todos, Topics: topics,
		MemoryTopics: memoryTopics, TodoTopics: todoTopics,
		LLM: llm, LLMModel: cfg.LLMFastModel,
		Prompt: string(promptBytes), PromptVersion: promptVersion,
		ExtractWindow: cfg.ExtractWindow,
		Gate:          memory.GateConfig{MinConf: cfg.QualityMinConf, TodoConf: cfg.QualityTodoConf},
		Voiceprint: voiceprintCli, Speakers: speakers, VoiceprintThreshold: cfg.VoiceprintThreshold,
	})
```

import 增 `"zhiwei/internal/storage"`、`"zhiwei/internal/voiceprint"`。

- [ ] **Step 4: 跑编译 + 单测**

Run: `go build ./cmd/zhiwei-server && go vet ./...`
Expected: 编译通过，无 vet 错误。

- [ ] **Step 5: 提交**

```bash
git add cmd/zhiwei-server/main.go internal/config/config.go internal/pipeline/
git commit -m "feat: 装配 file ASR + TOS + voiceprint + speaker stage"
```

---

## Phase 5：API

### Task 13：speaker API handler

**Files:**
- Create: `internal/api/speaker.go`
- Create: `internal/api/speaker_test.go`
- Modify: `internal/api/router.go`

- [ ] **Step 1: 写失败测试**

`internal/api/speaker_test.go`：

```go
package api

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
)

// fakeVoiceprintAPI 实现 speaker handler 依赖的 voiceprint.Client 子集（Embed/Add/Remove）。
type fakeVoiceprintAPI struct{}

func (fakeVoiceprintAPI) Embed(ctx context.Context, p string) ([]float32, error) { return make([]float32, 256), nil }
func (fakeVoiceprintAPI) Add(ctx context.Context, v []float32, id ids.ID) error  { return nil }
func (fakeVoiceprintAPI) Remove(ctx context.Context, id ids.ID) error             { return nil }
func (fakeVoiceprintAPI) Search(ctx context.Context, v []float32) (ids.ID, float64, bool, error) {
	return 0, 0, false, nil
}

func TestSpeakerRenameAndDelete(t *testing.T) {
	if testing.Short() { t.Skip("需要 MySQL") }
	ctx := context.Background()
	sp := &repo.Speaker{Name: "说话人xx", Source: "auto"}
	_ = testSpeakers.Create(ctx, sp)

	h := &SpeakerHandler{Speakers: testSpeakers, Voiceprint: fakeVoiceprintAPI{}, DataDir: t.TempDir()}

	// rename
	r := httptest.NewRequest("PATCH", "/api/speakers/"+sp.ID.String(), bytes.NewBufferString(`{"name":"张三"}`))
	w := httptest.NewRecorder()
	h.Rename(w, r)
	if w.Code != 200 { t.Fatalf("rename code %d", w.Code) }
	got, _ := testSpeakers.Get(ctx, sp.ID)
	if got.Name != "张三" { t.Fatalf("name=%s", got.Name) }

	// delete
	r2 := httptest.NewRequest("DELETE", "/api/speakers/"+sp.ID.String(), nil)
	w2 := httptest.NewRecorder()
	h.Delete(w2, r2)
	if w2.Code != 204 { t.Fatalf("delete code %d", w2.Code) }
}

func TestSpeakerEnroll(t *testing.T) {
	if testing.Short() { t.Skip("需要 MySQL") }
	h := &SpeakerHandler{Speakers: testSpeakers, Voiceprint: fakeVoiceprintAPI{}, DataDir: t.TempDir()}
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	fw, _ := mw.CreateFormFile("file", "sample.wav")
	fw.Write([]byte("RIFF\x00\x00\x00\x00WAVE")) // 假 wav 头
	_ = mw.WriteField("name", "李四")
	mw.Close()
	r := httptest.NewRequest("POST", "/api/speakers", body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.Enroll(w, r)
	if w.Code != 200 { t.Fatalf("enroll code %d body %s", w.Code, w.Body.String()) }
}
```

> `testSpeakers` 为 api 包测试 fixture（main_test.go 增 `testSpeakers = &repo.SpeakerRepo{DB: testDB}`）。

- [ ] **Step 2: 跑测试验证失败**

Run: `TEST_MYSQL_DSN="..." go test ./internal/api/ -run TestSpeaker -v`
Expected: FAIL（`SpeakerHandler` 未定义）。

- [ ] **Step 3: 写 handler**

`internal/api/speaker.go`：

```go
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/go-chi/chi/v5"

	"zhiwei/internal/ids"
	"zhiwei/internal/repo"
	"zhiwei/internal/voiceprint"
)

// SpeakerHandler 说话人名册 + 录入 + 换人。
type SpeakerHandler struct {
	Speakers   *repo.SpeakerRepo
	Transcripts *repo.TranscriptRepo
	Voiceprint voiceprint.Client
	DataDir    string
}

func RegisterSpeaker(r chi.Router, h *SpeakerHandler) {
	r.Get("/api/speakers", h.List)
	r.Post("/api/speakers", h.Enroll)
	r.Patch("/api/speakers/{id}", h.Rename)
	r.Delete("/api/speakers/{id}", h.Delete)
	r.Get("/api/sessions/{sid}/speakers", h.SessionSpeakers)
	r.Patch("/api/sessions/{sid}/segments/{seg}/speaker", h.ReassignSegment)
}

func (h *SpeakerHandler) List(w http.ResponseWriter, r *http.Request) {
	list, err := h.Speakers.List(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError); return
	}
	writeJSON(w, map[string]any{"speakers": list})
}

// Enroll 录入：收音频样本 + 名 → 转码 wav16k → sidecar /embed → 登记(enrolled) + /add。
func (h *SpeakerHandler) Enroll(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "解析失败: "+err.Error(), http.StatusBadRequest); return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "缺少 file", http.StatusBadRequest); return
	}
	defer file.Close()
	name := r.FormValue("name")
	if name == "" {
		http.Error(w, "缺少 name", http.StatusBadRequest); return
	}
	dir := filepath.Join(h.DataDir, "enroll")
	_ = os.MkdirAll(dir, 0o755)
	sid := ids.New()
	src := filepath.Join(dir, sid.String()+".wav")
	out, err := os.Create(src)
	if err != nil { http.Error(w, "存储失败", http.StatusInternalServerError); return }
	if _, err := io.Copy(out, file); err != nil {
		http.Error(w, "写入失败", http.StatusInternalServerError); return
	}
	out.Close()
	// 转码 16k mono（如已是 wav 可直接用；保险转一道）
	wav16 := filepath.Join(dir, sid.String()+"-16k.wav")
	if err := transcodeEnroll(src, wav16); err != nil {
		http.Error(w, "转码失败: "+err.Error(), http.StatusInternalServerError); return
	}
	vec, err := h.Voiceprint.Embed(r.Context(), wav16)
	if err != nil || len(vec) != 256 {
		http.Error(w, "声纹提取失败", http.StatusInternalServerError); return
	}
	sp := &repo.Speaker{Name: name, Source: "enrolled", Embedding: float32BlobAPI(vec), SampleCount: 1}
	if err := h.Speakers.Create(r.Context(), sp); err != nil {
		http.Error(w, "登记失败: "+err.Error(), http.StatusInternalServerError); return
	}
	_ = h.Voiceprint.Add(r.Context(), vec, sp.ID)
	_ = os.Remove(src); _ = os.Remove(wav16)
	writeJSON(w, sp)
}

func transcodeEnroll(src, dst string) error {
	cmd := exec.Command("ffmpeg", "-y", "-i", src, "-ar", "16000", "-ac", "1", "-sample_fmt", "s16", dst)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s", out)
	}
	return nil
}

func (h *SpeakerHandler) Rename(w http.ResponseWriter, r *http.Request) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil { http.Error(w, "invalid id", http.StatusBadRequest); return }
	var req struct{ Name string `json:"name"` }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		http.Error(w, "缺少 name", http.StatusBadRequest); return
	}
	if err := h.Speakers.UpdateName(r.Context(), id, req.Name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError); return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (h *SpeakerHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id, err := ids.ParseID(chi.URLParam(r, "id"))
	if err != nil { http.Error(w, "invalid id", http.StatusBadRequest); return
	}
	_ = h.Voiceprint.Remove(r.Context(), id)
	if err := h.Speakers.Delete(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError); return
	}
	// 清悬空引用（best-effort，单 SQL）
	_, _ = h.Speakers.DB.ExecContext(r.Context(),
		`UPDATE transcript_segment SET speaker_id = NULL WHERE speaker_id = ?`, id.Int64())
	w.WriteHeader(http.StatusNoContent)
}

// SessionSpeakers 本 session 解析到的说话人（面板用）。
func (h *SpeakerHandler) SessionSpeakers(w http.ResponseWriter, r *http.Request) {
	sid, err := ids.ParseID(chi.URLParam(r, "sid"))
	if err != nil { http.Error(w, "invalid id", http.StatusBadRequest); return
	}
	tr, err := h.Transcripts.GetBySession(r.Context(), sid)
	if err != nil { http.Error(w, "无转写", http.StatusNotFound); return
	}
	list, err := h.Transcripts.ListSpeakersForTranscript(r.Context(), tr.ID)
	if err != nil { http.Error(w, err.Error(), http.StatusInternalServerError); return
	}
	// color_index = 序号
	for i := range list {
		list[i].ColorIndex = i
	}
	writeJSON(w, map[string]any{"speakers": list})
}

// ReassignSegment 单段换人（前端"换人"下拉）。
func (h *SpeakerHandler) ReassignSegment(w http.ResponseWriter, r *http.Request) {
	sid, err := ids.ParseID(chi.URLParam(r, "sid"))
	if err != nil { http.Error(w, "invalid id", http.StatusBadRequest); return
	}
	segID, err := ids.ParseID(chi.URLParam(r, "seg"))
	if err != nil { http.Error(w, "invalid seg id", http.StatusBadRequest); return
	}
	tr, err := h.Transcripts.GetBySession(r.Context(), sid)
	if err != nil { http.Error(w, "无转写", http.StatusNotFound); return
	}
	var req struct{ SpeakerID string `json:"speaker_id"` }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求体非法", http.StatusBadRequest); return
	}
	spID, err := ids.ParseID(req.SpeakerID)
	if err != nil { http.Error(w, "invalid speaker_id", http.StatusBadRequest); return
	}
	if err := h.Transcripts.SetSegmentSpeakerByID(r.Context(), tr.ID, segID, spID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError); return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// float32BlobAPI 复用 pipeline 的实现；此处内联避免反向依赖（同 repo.RecomputeFullText 模式）。
func float32BlobAPI(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[i*4:], mathFloat32bitsAPI(x))
	}
	return b
}
func mathFloat32bitsAPI(f float32) uint32 { return math_Float32bits(f) }
var _ = strconv.Itoa // 避免未用 import
```

> 落地时：`mathFloat32bitsAPI` 改用 `math.Float32bits`（import `"math"`，删除自造 helper 与 `strconv` 占位）。`SpeakerInSegment` 的 `ColorIndex` 字段需在 `repo.SpeakerInSegment` 加 `ColorIndex int `json:"color_index" db:"-"``（Task 11 已定义结构体，此处补字段；db:"-" 因非 DB 列）。`SpeakerHandler.Delete` 用 `h.Speakers.DB` —— 需 `SpeakerRepo` 暴露 DB（已为 `struct{ DB *sqlx.DB }`，可直接访问）。

- [ ] **Step 4: 补 SpeakerInSegment.ColorIndex 字段**

`internal/repo/transcript.go` 的 `SpeakerInSegment` 加：

```go
type SpeakerInSegment struct {
	SpeakerID    ids.ID `db:"speaker_id" json:"speaker_id"`
	Name         string `db:"name" json:"name"`
	Source       string `db:"source" json:"source"`
	SegmentCount int    `db:"segment_count" json:"segment_count"`
	ColorIndex   int    `db:"-" json:"color_index"`
}
```

- [ ] **Step 5: 注册路由**

`internal/api/router.go` 的 `NewRouter` 末尾返回前不动；路由由 main 调 `RegisterSpeaker` 挂（Task 15）。或在 `NewRouter` 内不挂（保持 handler 注入风格）。

- [ ] **Step 6: 落地修正 + 跑测试**

修 `math.Float32bits` import；补 fixture。

Run: `TEST_MYSQL_DSN="..." go test ./internal/api/ -run TestSpeaker -v`
Expected: PASS。

- [ ] **Step 7: 提交**

```bash
git add internal/api/speaker.go internal/api/speaker_test.go internal/api/router.go internal/repo/transcript.go
git commit -m "feat(api): speaker 名册/录入/改名/删除/换人"
```

### Task 14：GetSession 增强

**Files:**
- Modify: `internal/api/query.go`
- Modify: `internal/api/query_test.go`

- [ ] **Step 1: 扩展 segmentView + GetSession**

`internal/api/query.go`：

```go
type segmentView struct {
	ID         string `json:"id"`
	Speaker    string `json:"speaker"`     // 显示名（解析到用登记名，否则 "说话人 N"）
	SpeakerID  string `json:"speaker_id,omitempty"`
	Text       string `json:"text"`
	StartMS    int64  `json:"start_ms"`
	EndMS      int64  `json:"end_ms"`
}
```

`GetSession` 内构造 views 处改为：

```go
		views := make([]segmentView, len(segs))
		for i, sg := range segs {
			views[i] = segmentView{
				ID: sg.ID.String(), Text: sg.Text, StartMS: sg.StartMS, EndMS: sg.EndMS,
			}
			if sg.SpeakerID != nil {
				views[i].SpeakerID = sg.SpeakerID.String()
				if sp, err := h.Speakers.Get(r.Context(), *sg.SpeakerID); err == nil {
					views[i].Speaker = sp.Name
					continue
				}
			}
			views[i].Speaker = speakerLabelName(sg.SpeakerLabel) // 回退 "说话人 N"/"未知说话人"
		}
		resp["segments"] = views
```

> `QueryHandler` 已有 `Transcripts`；需新增 `Speakers *repo.SpeakerRepo` 字段（main 注入）。`GetSession` 内 N+1 查 speaker（单 session 段数小，可接受；或先 ListSpeakersForTranscript 建表再映射——优化版用后者）。

优化版（避免 N+1）：

```go
		spMap := map[ids.ID]string{}
		if sis, err := h.Transcripts.ListSpeakersForTranscript(r.Context(), tr.ID); err == nil {
			for _, s := range sis {
				spMap[s.SpeakerID] = s.Name
			}
		}
		for i, sg := range segs {
			views[i] = segmentView{ID: sg.ID.String(), Text: sg.Text, StartMS: sg.StartMS, EndMS: sg.EndMS}
			if sg.SpeakerID != nil {
				views[i].SpeakerID = sg.SpeakerID.String()
				if name, ok := spMap[*sg.SpeakerID]; ok {
					views[i].Speaker = name
					continue
				}
			}
			views[i].Speaker = speakerLabelName(sg.SpeakerLabel)
		}
		resp["segments"] = views
		resp["speakers"] = spMap // 或 ListSpeakersForTranscript 结果
```

`QueryHandler` 加字段：

```go
type QueryHandler struct {
	Sessions    *repo.SessionRepo
	Jobs        *repo.JobRepo
	Transcripts *repo.TranscriptRepo
	Memories    *repo.MemoryRepo
	Todos       *repo.TodoRepo
	Speakers    *repo.SpeakerRepo // 新增
}
```

- [ ] **Step 2: query_test.go 增断言**

加一个测试：seed speaker + segment.speaker_id，GET /api/sessions/{id} 返回 segment.speaker=登记名 + speakers 非空。（复用现有 router_test 模式。）

- [ ] **Step 3: 跑测试 + 提交**

Run: `TEST_MYSQL_DSN="..." go test ./internal/api/ -run TestGetSession -v`
Expected: PASS。

```bash
git add internal/api/query.go internal/api/query_test.go
git commit -m "feat(api): GetSession 附 segment.speaker 名 + speakers 列表"
```

### Task 15：router + main 装配 API

**Files:**
- Modify: `cmd/zhiwei-server/main.go`

- [ ] **Step 1: main 挂 SpeakerHandler + 注入 QueryHandler.Speakers**

```go
	api.RegisterSpeaker(r, &api.SpeakerHandler{
		Speakers: speakers, Transcripts: transcripts,
		Voiceprint: voiceprintCli, DataDir: cfg.DataDir,
	})
	api.RegisterQuery(r, &api.QueryHandler{
		Sessions: sessions, Jobs: jobs, Transcripts: transcripts,
		Memories: memories, Todos: todos, Speakers: speakers,
	})
```

- [ ] **Step 2: 编译 + 提交**

Run: `go build ./cmd/zhiwei-server`
Expected: 通过。

```bash
git add cmd/zhiwei-server/main.go
git commit -m "feat: 装配 speaker API"
```

---

## Phase 6：前端

### Task 16：说话人面板 + 录入 + 换人

**Files:**
- Modify: `web/index.html`
- Modify: `web/app.js`

- [ ] **Step 1: index.html 加说话人面板模板**

在转写详情卡片 `<audio ...>` 之后、`<div class="kv"><b>转写详情</b>...` 之前插入：

```html
          <!-- 说话人面板 -->
          <div class="speaker-row" style="margin:6px 0 10px; display:flex; flex-wrap:wrap; gap:6px; align-items:center">
            <span class="muted" style="margin-right:4px">说话人</span>
            <span v-for="(sp, i) in detail.speakers" :key="sp.speaker_id"
                  class="chip" :style="{background: speakerColor(sp, i).bg, color: speakerColor(sp, i).fg}"
                  style="cursor:pointer" @click="toggleSpeakerFilter(sp.speaker_id)"
                  :title="sp.source==='auto' ? '自动登记' : '手动录入'">
              {{ sp.name }}
              <button class="chip-x" @click.stop="startRenameSpeaker(sp)" title="改名">✎</button>
              <button class="chip-x" @click.stop="askDeleteSpeaker(sp)" title="删除">✕</button>
            </span>
            <label class="chip-add" v-if="!enrollOpen" @click="openEnroll">+ 新加说话人</label>
            <span class="chip" v-if="speakerFilter" style="cursor:pointer; opacity:.8" @click="speakerFilter=null">显示全部 ✕</span>
          </div>
          <!-- 改名行 -->
          <div v-if="renamingSpeaker" class="edit-form" style="margin:0 0 8px">
            <input class="txt inline" v-model="renamingSpeaker.name" @keyup.enter="commitRenameSpeaker">
            <button class="btn primary" style="padding:6px 12px; margin-left:6px" @click="commitRenameSpeaker">保存</button>
            <button class="btn mini" @click="renamingSpeaker=null">取消</button>
          </div>
          <!-- 录入表单 -->
          <div class="card sunken" v-if="enrollOpen" style="margin-bottom:10px">
            <div class="kv"><b>录入说话人</b><button class="btn-link" @click="enrollOpen=false">✕</button></div>
            <label class="field-label">名称</label>
            <input class="txt" v-model="enrollForm.name" placeholder="如：张三" style="margin-bottom:8px">
            <label class="field-label">语音样本（拖拽/选择 5-30s 干净人声）</label>
            <div id="drop" @dragover.prevent @drop.prevent="onEnrollDrop" style="padding:18px; margin-bottom:8px">
              {{ enrollForm.file ? enrollForm.file.name : '拖拽 wav/mp3 到此处' }}
            </div>
            <div class="edit-actions">
              <button class="btn primary" :disabled="!enrollForm.name.trim()||!enrollForm.file||enrolling" @click="submitEnroll">
                <span v-if="enrolling" class="spinner"></span>{{ enrolling?'录入中…':'录入' }}
              </button>
              <button class="btn mini" @click="enrollOpen=false">取消</button>
            </div>
          </div>
```

段 badge 改为可换人下拉（`v-for` 段处）：

```html
          <div v-for="(sg, i) in visibleSegments" :key="i" class="seg" v-show="!speakerFilter || sg.speaker_id===speakerFilter">
            <select class="sp-select" :class="spClass(sg.speaker_id||sg.speaker)" :style="{background: segSpeakerBg(sg)}"
                    @change="reassignSegment(sg, $event.target.value)" style="font-size:var(--fs-xs); color:#fff; border:none; border-radius:6px; padding:2px 6px; cursor:pointer">
              <option :value="sg.speaker_id">{{ sg.speaker }}</option>
              <option v-for="sp in allSpeakers" :key="sp.id" :value="sp.id">{{ sp.name }}</option>
              <option value="__new">+ 新加…</option>
            </select>
            <div class="seg-text"> ... (不变) ... </div>
          </div>
```

> `visibleSegments` = `detail.segments`（filter 用 v-show 控制，不裁剪数组以保留序号）。`allSpeakers` = 全部已登记说话人（loadSessions 时一并拉 `/api/speakers`）。

- [ ] **Step 2: app.js 加状态 + 方法**

`web/app.js` 在 setup 内（detail 附近）加：

```js
    const speakerFilter = ref(null);
    const renamingSpeaker = ref(null);
    const enrollOpen = ref(false);
    const enrollForm = reactive({ name: '', file: null });
    const enrolling = ref(false);
    const allSpeakers = ref([]);

    const SPEAKER_PALETTE = [
      {bg:'#4338ca',fg:'#fff'},{bg:'#0e7490',fg:'#fff'},{bg:'#b45309',fg:'#fff'},
      {bg:'#6d28d9',fg:'#fff'},{bg:'#047857',fg:'#fff'},{bg:'#be123c',fg:'#fff'},
      {bg:'#1e40af',fg:'#fff'},{bg:'#9d174d',fg:'#fff'},
    ];
    function speakerColor(sp, i) { return SPEAKER_PALETTE[i % SPEAKER_PALETTE.length]; }
    function segSpeakerBg(sg) {
      if (sg.speaker_id && detail.value?.speakers) {
        const idx = detail.value.speakers.findIndex(s => s.speaker_id===sg.speaker_id);
        if (idx>=0) return SPEAKER_PALETTE[idx % SPEAKER_PALETTE.length].bg;
      }
      return '#9a9388';
    }
    function toggleSpeakerFilter(id) { speakerFilter.value = speakerFilter.value===id ? null : id; }

    function openEnroll() { enrollOpen.value=true; enrollForm.name=''; enrollForm.file=null; }
    function onEnrollDrop(e) { enrollForm.file = e.dataTransfer.files[0]; }
    async function submitEnroll() {
      const fd = new FormData();
      fd.append('file', enrollForm.file);
      fd.append('name', enrollForm.name);
      enrolling.value = true;
      try {
        await api('POST', '/api/speakers', fd);
        await loadAllSpeakers();
        enrollOpen.value = false;
        toast.value = '已录入说话人'; setTimeout(()=>toast.value='', 2000);
      } catch(e) { showError(e); }
      enrolling.value = false;
    }
    async function loadAllSpeakers() {
      const d = await api('GET', '/api/speakers'); allSpeakers.value = d.speakers||[];
    }
    function startRenameSpeaker(sp) { renamingSpeaker.value = {id: sp.speaker_id, name: sp.name}; }
    async function commitRenameSpeaker() {
      if (!renamingSpeaker.value) return;
      await api('PATCH', '/api/speakers/'+renamingSpeaker.value.id, {name: renamingSpeaker.value.name});
      renamingSpeaker.value = null;
      await reloadSession(detail.value.session.id);
    }
    async function askDeleteSpeaker(sp) {
      if (!confirm('删除说话人「'+sp.name+'」？关联段将变为未解析。')) return;
      await api('DELETE', '/api/speakers/'+sp.speaker_id);
      await loadAllSpeakers();
      await reloadSession(detail.value.session.id);
    }
    async function reassignSegment(sg, val) {
      if (val==='__new') { openEnroll(); return; }
      await api('PATCH', `/api/sessions/${detail.value.session.id}/segments/${sg.id}/speaker`, {speaker_id: val});
      await reloadSession(detail.value.session.id);
    }
```

`reloadSession` 后调 `loadAllSpeakers`（已在 loadSessions 时调一次）。`return` 块补暴露：`speakerFilter, renamingSpeaker, enrollOpen, enrollForm, enrolling, allSpeakers, speakerColor, segSpeakerBg, toggleSpeakerFilter, openEnroll, onEnrollDrop, submitEnroll, startRenameSpeaker, commitRenameSpeaker, askDeleteSpeaker, reassignSegment`。

`api()` 函数需支持 FormData（不设 Content-Type，让浏览器带 boundary）。检查 `api(method,url,body)`：若 body 是 FormData，跳过 JSON 序列化与 Content-Type。

- [ ] **Step 3: app.js api() 兼容 FormData**

定位 `async function api(method, url, body)`，改：

```js
    async function api(method, url, body) {
      const opts = { method, headers: {} };
      if (body instanceof FormData) { opts.body = body; }
      else if (body !== undefined) { opts.headers['Content-Type']='application/json'; opts.body = JSON.stringify(body); }
      const res = await fetch(url, opts);
      if (!res.ok) {
        let msg = res.status+'';
        try { const t = await res.text(); if (t) msg = t; } catch {}
        throw new Error(msg);
      }
      if (res.status===204) return null;
      return res.json();
    }
```

- [ ] **Step 4: hash-web + 手动验证**

Run: `make hash-web && make build`
打开 `http://localhost:8080`，上传一段多人录音，等处理完，展开详情：
- 说话人面板出现 chips，点击过滤生效，✎ 改名生效，+ 新加说话人录入生效，段下拉换人生效。

- [ ] **Step 5: 提交**

```bash
git add web/index.html web/app.js web/app.*.js
git commit -m "feat(web): 说话人面板 过滤/改名/换人/录入"
```

---

## Phase 7：配置/Makefile/文档/e2e

### Task 17：Makefile + README + asr-protocol-notes

**Files:**
- Modify: `Makefile`
- Modify: `README.md`
- Modify: `docs/superpowers/specs/asr-protocol-notes.md`

- [ ] **Step 1: Makefile 加目标**

```make
.PHONY: sidecar-start sidecar-stop sidecar-restart sidecar-status spike-voiceprint

sidecar-start:
	bash scripts/sidecar.sh start
sidecar-stop:
	bash scripts/sidecar.sh stop
sidecar-restart:
	bash scripts/sidecar.sh restart
sidecar-status:
	bash scripts/sidecar.sh status

spike-voiceprint:
	python services/voiceprint/spike.py testdata/speech.wav
```

`scripts/sidecar.sh`（新建，仿 `scripts/dev.sh`）：

```bash
#!/usr/bin/env bash
# 声纹 sidecar 后台启停（uvicorn）。PID 文件在 data/voiceprint.pid。
set -e
PIDF=data/voiceprint.pid
CMD="uvicorn services.voiceprint.app:app --host 127.0.0.1 --port 8010"
case "${1:-status}" in
  start) mkdir -p data; [ -f "$PIDF" ] && kill -0 $(cat "$PIDF") 2>/dev/null && { echo "已运行 $(cat $PIDF)"; exit 0; }
    nohup $CMD >data/voiceprint.log 2>&1 & echo $! >"$PIDF"; echo "started $PIDF";;
  stop) [ -f "$PIDF" ] && kill $(cat "$PIDF") 2>/dev/null; rm -f "$PIDF"; echo "stopped";;
  restart) $0 stop; $0 start;;
  status) [ -f "$PIDF" ] && kill -0 $(cat "$PIDF") 2>/dev/null && echo "running $(cat $PIDF)" || echo "stopped";;
esac
```

- [ ] **Step 2: README 补 env + 启动顺序**

`README.md` 环境要求/快速开始增：

```
- 环境变量（新增）：
  - `TOS_ACCESS_KEY` / `TOS_SECRET_KEY`（火山引擎对象存储 TOS，ASR 文件接口需上传音频；可放 .env）
  - `ZW_VOICEPRINT_SIDECAR_URL`（默认 http://127.0.0.1:8010）
  - `ZW_VOICEPRINT_THRESHOLD`（默认 0.5，余弦，需实测调）
- 启动顺序：MySQL(`make compose-up`) → 声纹 sidecar(`make sidecar-start`) → 服务(`make dev`)
```

API 一览增：

```
GET/POST/PATCH/DELETE /api/speakers   说话人名册 / 录入 / 改名 / 删除
GET /api/sessions/{id}/speakers        会话内说话人列表
PATCH /api/sessions/{id}/segments/{seg}/speaker  段换人
```

- [ ] **Step 3: 更新 asr-protocol-notes**

`docs/superpowers/specs/asr-protocol-notes.md` 顶部加一节「2026-08-22 更新：改用异步文件 ASR」，记录 `/file/submit`+`/file/query`、`show_utterances`/`enable_speaker_info`、`spk_N`+ms、TOS presigned URL、弃用 realtime。引用本设计文档。

- [ ] **Step 4: 提交**

```bash
git add Makefile scripts/sidecar.sh README.md docs/superpowers/specs/asr-protocol-notes.md
git commit -m "chore: Makefile sidecar/spike 目标 + README env + ASR 笔记更新"
```

### Task 18：e2e 扩展 + 全量回归

**Files:**
- Modify: `scripts/e2e.sh`

- [ ] **Step 1: e2e 校验 speaker_id**

`scripts/e2e.sh` 在轮询到 completed 后增断言：

```bash
# 校验 segment 带 speaker_id
SPEAKERS=$(curl -s "http://localhost:8080/api/sessions/$SID/speakers")
echo "speakers: $SPEAKERS"
echo "$SPEAKERS" | grep -q '"speaker_id"' || { echo "FAIL: 无解析到的说话人"; exit 1; }
```

- [ ] **Step 2: 跑全量测试**

Run: `make test && make test-integration && make e2e`
Expected: 全 PASS（e2e 需真实 STEPFUN/TOS/WeSpeaker，手动跑）。

- [ ] **Step 3: 最终提交**

```bash
git add scripts/e2e.sh
git commit -m "test(e2e): 校验 segment speaker_id"
```

---

## 自审记录

- **Spec 覆盖**：6 条需求 → Task 映射：①录入(Task 13 Enroll + 16 录入表单) ②ASR 时间戳+说话人(Task 5/6) ③ffmpeg 切片(Task 11 sliceAudio) ④WeSpeaker 256 维(Task 2 spike/Task 9 sidecar) ⑤FAISS 1:N+自动登记(Task 11 stage) ⑥timeline 切换/新加(Task 16)。✓
- **类型一致**：`SpeakerRepo`、`voiceprint.Client`、`SpeakerInSegment`、`segmentView`、`StageDeps` 字段跨任务一致；`ids.ID`/`ids.ParseID` 用法与现有一致。✓
- **占位符**：TOS/WeSpeaker 真实字段名显式由 spike(Task 1/2) 验证后固化，非臆造；`math.Float32bits` 等已在对应 Task 标注落地替换。✓
- **遗留**：Task 11/13 的 `mathFloat32bits`/`sqrtF32`/`asUnsafePointer` 自造 helper 必须在落地时换为 `math.Float32bits`/`math.Sqrt`（Task Step 8/6 已指示）。
