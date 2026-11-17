# 短变量让命令更清爽：所有目标都只跑一个 go 命令
BIN  := bin/api
PKG  := ./...
ADDR ?= :8080

.PHONY: help run build test test-race vet fmt fmt-check cover tidy clean examples

help: ## 显示所有可用命令
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

examples: ## 运行并发示例（go run ./examples/goroutine）
	go run ./examples/goroutine

run: ## 启动服务（默认 :8080，可用 make run ADDR=:9000 覆盖）
	APP_ADDR=$(ADDR) go run ./cmd/api

build: ## 编译成单个二进制 -> bin/api
	go build -trimpath -o $(BIN) ./cmd/api

test: ## 跑全部测试
	go test $(PKG)

test-race: ## 带竞态检测跑测试（并发代码必跑）
	go test -race -count=1 $(PKG)

vet: ## 静态检查（相当于半个 linter）
	go vet $(PKG)

fmt: ## 格式化所有代码
	gofmt -w .

fmt-check: ## 检查格式是否符合 gofmt（CI 用）
	@out="$$(gofmt -l .)"; \
	if [ -n "$$out" ]; then echo "以下文件未格式化: $$out"; exit 1; fi; \
	echo "gofmt OK"

cover: ## 生成测试覆盖率报告 -> coverage.html
	go test -coverprofile=coverage.out -covermode=atomic $(PKG)
	go tool cover -html=coverage.out -o coverage.html
	@echo "已生成 coverage.html，用浏览器打开"

tidy: ## 整理 go.mod / go.sum
	go mod tidy

clean: ## 清理编译和覆盖率产物
	rm -rf bin coverage.out coverage.html
