.PHONY: build dev dev-start dev-stop dev-restart dev-status dev-logs test test-integration migrate-up migrate-down compose-up compose-down init-testdb spike-llm spike-embed spike-asr spike-voiceprint e2e sidecar-start sidecar-stop sidecar-restart sidecar-status sidecar-logs

# 给 web/app.js 生成内容 hash 文件名（缓存破除，无构建方案）。
# build / dev-start / dev-restart 自动依赖：改完 app.js 后任一入口都会重算指纹，
# 避免出现「index.html 引用旧 hash 副本 + 新模板」的 hasNameCandidates 未定义类事故
# （静态文件从磁盘实时 serve，不依赖编译，但指纹副本与 index.html 引用必须同步重算）。
hash-web:
	bash scripts/hash-web.sh

build: hash-web
	go build -o bin/zhiwei-server ./cmd/zhiwei-server

dev: build
	./bin/zhiwei-server

# 调试进程后台管理（scripts/dev.sh 封装）；start/restart 先重算 web 指纹再起服务
dev-start: hash-web
	bash scripts/dev.sh start
dev-stop:
	bash scripts/dev.sh stop
dev-restart: hash-web
	bash scripts/dev.sh restart
dev-status:
	bash scripts/dev.sh status
dev-logs:
	bash scripts/dev.sh logs

test:
	go test ./...

compose-up:
	docker compose -f deploy/docker-compose.yml up -d

compose-down:
	docker compose -f deploy/docker-compose.yml down

migrate-up:
	migrate -path migrations -database "mysql://zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei" up

migrate-down:
	migrate -path migrations -database "mysql://zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei" down 1

# 重建集成测试库（每次干净状态）+ 授权。
# F6 完整并行：真正的测试库是按包隔离的 zhiwei_test_<pkg>，由 repotest.DSN 懒建。
# 本目标负责三件事：①清理上一轮遗留的 zhiwei_test_<pkg>（逐个 DROP，防脏 schema）；
# ②给 zhiwei 用户授予 `zhiwei_test_%`.* 通配符权限（MySQL 通配符 GRANT 对「尚不存在」
# 的库也生效，故懒建时 zhiwei 已有全权限）；③兜底建共享库 zhiwei_test + 迁移（旧流程/
# 手动排查仍可用）。
init-testdb:
	docker exec zhiwei-mvp-mysql bash -c 'for db in $$(mysql -uroot -proot -N -e "SHOW DATABASES LIKE \"zhiwei_test_%\""); do mysql -uroot -proot -e "DROP DATABASE IF EXISTS \`$$db\`"; done'
	docker exec zhiwei-mvp-mysql mysql -uroot -proot -e "DROP DATABASE IF EXISTS zhiwei_test; CREATE DATABASE zhiwei_test CHARACTER SET utf8mb4; GRANT ALL PRIVILEGES ON zhiwei_test.* TO 'zhiwei'@'%'; GRANT ALL PRIVILEGES ON \`zhiwei_test_%\`.* TO 'zhiwei'@'%'; FLUSH PRIVILEGES;"
	migrate -path migrations -database "mysql://root:root@tcp(127.0.0.1:3307)/zhiwei_test" up

# 集成测试：需要 docker compose 里的 MySQL 已启动；init-testdb 会清库并配好通配符授权。
# 无需 -p 1：各测试包经 repotest.DSN 懒建独立库 zhiwei_test_<pkg>，并行跑包时各用各的库、
# 互不可见，两个并行根因（ClaimNext 全局抢 pending job、extract user_id=1 跨包去重污染）
# 均由库级隔离消解。
test-integration: init-testdb
	TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./...

spike-llm:
	go run ./cmd/spike/llm
spike-embed:
	go run ./cmd/spike/embed
spike-asr:
	go run ./cmd/spike/asr testdata/speech.wav

e2e:
	bash scripts/e2e.sh

# 声纹 sidecar（Python FastAPI，WeSpeaker+FAISS）后台启停。PID 文件在 data/voiceprint.pid。
# 首次启动前需建 venv：bash scripts/setup-voiceprint.sh（一键装 wespeaker + 依赖）
sidecar-start:
	bash scripts/sidecar.sh start
sidecar-stop:
	bash scripts/sidecar.sh stop
sidecar-restart:
	bash scripts/sidecar.sh restart
sidecar-status:
	bash scripts/sidecar.sh status
sidecar-logs:
	bash scripts/sidecar.sh logs

# 验证 WeSpeaker resnet34-LM 加载 + 输出 256 维（手动，需装好 venv 与模型权重）
spike-voiceprint:
	services/voiceprint/.venv/bin/python services/voiceprint/spike.py testdata/speech.wav
