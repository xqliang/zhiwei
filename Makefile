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
