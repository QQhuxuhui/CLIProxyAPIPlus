.PHONY: build build-web build-go dev dev-web dev-go clean install-web help all

# 默认目标
all: build

# 安装前端依赖
install-web:
	cd web && npm install

# 构建前端
build-web: install-web
	cd web && npm run build
	mkdir -p internal/managementasset/static
	cp web/dist/index.html internal/managementasset/static/management.html
	@echo "Frontend built successfully"

# 构建后端
build-go:
	go build -o bin/server ./cmd/server
	@echo "Backend built successfully"

# 构建全部（先前端后后端）
build: build-web build-go
	@echo "Full build completed"

# 前端开发模式
dev-web:
	cd web && npm run dev

# 后端开发模式
dev-go:
	go run ./cmd/server

# 清理
clean:
	rm -rf bin/
	rm -rf web/dist/
	rm -rf web/node_modules/
	rm -f internal/managementasset/static/management.html
	@echo "Cleaned up build artifacts"

# 帮助
help:
	@echo "Available targets:"
	@echo "  make build       - Build frontend and backend"
	@echo "  make build-web   - Build frontend only"
	@echo "  make build-go    - Build backend only"
	@echo "  make dev-web     - Start frontend dev server (port 5173)"
	@echo "  make dev-go      - Start backend dev server (port 8080)"
	@echo "  make clean       - Clean build artifacts"
	@echo "  make install-web - Install frontend dependencies"
