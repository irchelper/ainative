.PHONY: build build-web dev dev-api test test-go test-web clean clean-all vet

BIN := agent-queue

# 前端构建（输出到 internal/webui/dist/）
build-web:
	cd web && npm ci && npm run build

# Go 构建（含 embed 前端，先跑 build-web）
build: build-web
	go build -o $(BIN) ./cmd/server

# 开发模式说明
dev:
	@echo "启动 Go API server（终端1）: make dev-api"
	@echo "启动 Vite dev server（终端2）: cd web && npm run dev"

dev-api:
	go run ./cmd/server --static-dir=internal/webui/dist

# 测试
test: test-go test-web

test-go:
	go test -race ./...

test-web:
	cd web && npm test

# 代码检查
vet:
	go vet ./...

# 清理（保留数据库）
clean:
	rm -f $(BIN)

# 完全清理
clean-all: clean
	rm -rf data/queue.db internal/webui/dist/ web/node_modules/

run:
	go run ./cmd/server
