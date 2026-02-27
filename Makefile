.PHONY: build build-web test test-go test-web clean clean-all

BIN := agent-queue

# 前端构建
build-web:
	cd web && npm ci && npm run build

# Go 构建（含 embed 前端，前端就绪后使用；当前无 web/ 目录时直接 go build）
build:
	go build -o $(BIN) ./cmd/server

# 测试
test: test-go

test-go:
	go test -race ./...

# 清理
clean:
	rm -f $(BIN)

clean-all:
	rm -f $(BIN) data/queue.db

run:
	go run ./cmd/server

vet:
	go vet ./...
