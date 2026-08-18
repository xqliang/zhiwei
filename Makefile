.PHONY: build dev test test-integration migrate-up migrate-down compose-up compose-down init-testdb spike-llm spike-embed spike-asr e2e

build:
	go build -o bin/zhiwei-server ./cmd/zhiwei-server

dev: build
	./bin/zhiwei-server

test:
	go test ./...

compose-up:
	docker compose -f deploy/docker-compose.yml up -d

compose-down:
	docker compose -f deploy/docker-compose.yml down

migrate-up:
	migrate -path migrations -database "mysql://zhiwei:zhiwei@tcp(127.0.0.1:3306)/zhiwei" up

migrate-down:
	migrate -path migrations -database "mysql://zhiwei:zhiwei@tcp(127.0.0.1:3306)/zhiwei" down 1

# 创建集成测试库并跑迁移
init-testdb:
	docker exec zhiwei-mysql mysql -uroot -proot -e "CREATE DATABASE IF NOT EXISTS zhiwei_test CHARACTER SET utf8mb4;"
	migrate -path migrations -database "mysql://root:root@tcp(127.0.0.1:3306)/zhiwei_test" up

# 集成测试：需要 docker compose 里的 MySQL 已启动并完成迁移
test-integration: init-testdb
	TEST_MYSQL_DSN="zhiwei:zhiwei@tcp(127.0.0.1:3306)/zhiwei_test?parseTime=true&charset=utf8mb4&multiStatements=true" go test ./...
