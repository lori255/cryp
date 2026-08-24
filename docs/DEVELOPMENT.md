# 开发与测试

## 前置条件

- Go 1.24
- Node.js 22 与 npm
- 支持 CGO 的 C 编译器（SQLite 驱动需要）
- FFmpeg；HEIF 缩略图开发还需要 `heif-convert`

## 本地运行

安装前端依赖：

```bash
cd web
npm ci
```

在仓库根目录启动后端：

```bash
mkdir -p .dev/config .dev/vaults .dev/source
PORT=9527 \
DATA_DIR="$PWD/.dev/config" \
VAULT_DIR="$PWD/.dev/vaults" \
SOURCE_DIR="$PWD/.dev/source" \
go run ./cmd/server
```

另开一个终端启动前端：

```bash
cd web
npm run dev
```

Vite 开发服务器会把 `/api` 代理到 `http://localhost:9527`。本地 `.dev/` 数据、`.env` 和编辑器文件均不应提交。

## 验证命令

提交 Pull Request 前至少运行：

```bash
go test ./... -race -count=1
go vet ./...
cd web
npm run lint
npm run build
```

生产镜像验证：

```bash
docker build -t cryp:dev .
docker compose config
```

真实视频转码、VAAPI、iOS/PWA 和大文件行为依赖外部环境，单元测试不能完全覆盖。相关修改应在 Pull Request 中说明测试设备、浏览器、媒体格式和回退路径。

## 代码位置

- `internal/crypto`：加密格式和流式 I/O
- `internal/api`：HTTP、内容 Range、HLS 和关停生命周期
- `internal/task`：上传、导入和索引任务
- `internal/thumbnail`：FFmpeg/HEIF 缩略图
- `web/src/components`：可复用 UI 与媒体生命周期
- `web/src/pages`：页面级状态和路由

不要在公开仓库提交真实保险库、数据库、媒体样本、日志、性能 profile、内部审核记录或本地代理状态。
