.PHONY: build dev dev-start dev-stop dev-restart dev-status dev-logs test test-integration migrate-up migrate-down compose-up compose-down init-testdb spike-llm spike-embed spike-asr spike-voiceprint e2e sidecar-start sidecar-stop sidecar-restart sidecar-status

# 给 web/app.js 生成内容 hash 文件名（缓存破除，无构建方案）。build 自动依赖。
hash-web:
	bash scripts/hash-web.sh

build: hash-web
	go build -o bin/zhiwei-server ./cmd/zhiwei-server

dev: build
	./bin/zhiwei-server

# 调试进程后台管理（scripts/dev.sh 封装）
dev-start:
	bash scripts/dev.sh start
dev-stop:
	bash scripts/dev.sh stop
dev-restart:
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

# 重建集成测试库（每次干净状态）并跑迁移
init-testdb:
	docker exec zhiwei-mvp-mysql mysql -uroot -proot -e "DROP DATABASE IF EXISTS zhiwei_test; CREATE DATABASE zhiwei_test CHARACTER SET utf8mb4; GRANT ALL PRIVILEGES ON zhiwei_test.* TO 'zhiwei'@'%'; FLUSH PRIVILEGES;"
	migrate -path migrations -database "mysql://root:root@tcp(127.0.0.1:3307)/zhiwei_test" up

# 集成测试：需要 docker compose 里的 MySQL 已启动并完成迁移。
# -p 1 串行执行各包测试：repo 的 TestJobLifecycle 与 pipeline 的 pool 测试
# 都依赖「领取最老的 pending 任务」，并行包二进制共享同一测试库会互相抢占任务。
test-integration: init-testdb
	TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3307)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test -p 1 ./...

spike-llm:
	go run ./cmd/spike/llm
spike-embed:
	go run ./cmd/spike/embed
spike-asr:
	go run ./cmd/spike/asr testdata/speech.wav

e2e:
	bash scripts/e2e.sh

# 声纹 sidecar（Python FastAPI，WeSpeaker+FAISS）后台启停。PID 文件在 data/voiceprint.pid。
# 启动前需在 services/voiceprint/ 建好 venv：python3 -m venv services/voiceprint/.venv &&
#   services/voiceprint/.venv/bin/pip install -r services/voiceprint/requirements.txt
sidecar-start:
	bash scripts/sidecar.sh start
sidecar-stop:
	bash scripts/sidecar.sh stop
sidecar-restart:
	bash scripts/sidecar.sh restart
sidecar-status:
	bash scripts/sidecar.sh status

# 验证 WeSpeaker resnet34-LM 加载 + 输出 256 维（手动，需装好 venv 与模型权重）
spike-voiceprint:
	services/voiceprint/.venv/bin/python services/voiceprint/spike.py testdata/speech.wav
