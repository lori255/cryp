# Cryp

一个自托管的加密文件保险库，类似 Cryptomator，通过 Web 界面实时流式解密浏览图片和视频，无需先解密到本地。

## 特性

- **AES-256-GCM 分块加密** — 文件内容按 32KB 分块加密，支持随机访问（视频拖动）
- **AES-SIV 文件名加密** — 文件名和目录名完全加密，支持长文件名自动缩短（`.c9s`）
- **实时流式解密** — 图片和视频直接在浏览器中查看/播放，不生成临时文件
- **Range 请求支持** — 视频可拖动进度条，按需解密对应片段
- **极低内存占用** — 处理 7GB 文件峰值内存约 57MB，`sync.Pool` 复用 buffer，`FADV_DONTNEED` 控制 page cache
- **后台任务系统** — 目录导入加密、文件上传均在后台执行，实时进度反馈
- **移动端适配** — iOS Safari PWA 支持，视频全屏横屏播放，触控友好

## 架构

```
┌─────────────────────────────────────────┐
│  React + Vite + TailwindCSS             │
│  Artplayer 视频播放 / react-photo-view  │
└─────────────┬───────────────────────────┘
              │ HTTP API
┌─────────────┴───────────────────────────┐
│  Go + Gin                               │
│  流式加解密 / Range 请求 / 任务管理      │
│  SQLite (WAL) / Session 管理            │
└─────────────┬───────────────────────────┘
              │ 文件系统
┌─────────────┴───────────────────────────┐
│  加密文件存储                            │
│  content.c9r / dirid.c9r / name.c9r     │
└─────────────────────────────────────────┘
```

## 快速开始

### Docker 部署（推荐）

```bash
git clone https://github.com/lori255/cryp.git
cd cryp
docker compose pull
docker compose up -d
```

访问 `http://localhost:9527`

### docker-compose.yml

```yaml
services:
  cryp:
    image: lori255/cryp:latest
    container_name: cryp
    ports:
      - "9527:9527"
    volumes:
      - /your/data/path:/data
    # Optional GPU acceleration for thumbnail generation (Linux VAAPI)
    # Enable only when host has /dev/dri/renderD128 and proper VAAPI driver stack.
    # devices:
    #   - /dev/dri/renderD128:/dev/dri/renderD128
    # group_add:
    #   - "105" # render device group GID on host (adjust to your host)
    environment:
      - PORT=9527
      - DATA_DIR=/data/config
      - VAULT_DIR=/data/vaults
      - GOMEMLIMIT=256MiB
      # Optional: thumbnail hardware acceleration strategy (auto/vaapi/qsv/cuda/cpu)
      # - CRYP_FFMPEG_HWACCEL=auto
    restart: unless-stopped
```

将 `/your/data/path` 替换为实际存储路径，保险库配置和加密文件都存储在此目录中。

### 首次使用

1. 打开浏览器访问 `http://<IP>:9527`
2. 输入保险库名称和密码创建新的保险库
3. 通过名称和密码登录进入保险库
4. 上传文件或导入本地目录

## 加密方案

| 组件 | 算法 | 说明 |
|------|------|------|
| 密钥派生 | scrypt | 从密码派生主密钥 |
| 密钥包装 | AES Key Wrap (RFC 3394) | 加密存储主密钥和 MAC 密钥 |
| 文件内容 | AES-256-GCM | 32KB 分块加密，每块独立 nonce |
| 文件名 | AES-SIV (RFC 5297) | 确定性加密，使用目录 ID 作为 AAD |
| 长文件名 | SHA-256 + Base64 | 超过 220 字符的加密名自动缩短 |

## 项目结构

```
cryp/
├── cmd/server/          # 入口、静态文件嵌入
├── internal/
│   ├── api/             # HTTP 路由和处理器
│   ├── crypto/          # 加解密核心（内容、文件名、密钥、IO）
│   ├── session/         # 会话管理
│   ├── storage/         # SQLite 数据库
│   └── task/            # 后台任务管理
├── web/                 # React 前端
│   └── src/
│       ├── components/  # UI 组件
│       ├── pages/       # 页面
│       └── lib/         # API 客户端
├── Dockerfile           # 三阶段构建
└── docker-compose.yml
```

## 内存优化

针对 Docker 容器环境做了专门的内存优化：

- **零分配流式加解密** — `EncryptingWriter` / `DecryptingReader` 复用内部 buffer
- **sync.Pool** — `DecryptingReader` 和 32KB copy buffer 跨请求复用
- **Page Cache 控制** — `FADV_SEQUENTIAL | FADV_NOREUSE` 预声明 + `FADV_DONTNEED` 主动释放
- **GOMEMLIMIT** — 限制 Go 堆上限为 256MB，触发更积极的 GC

## GPU 加速（可选）

当前版本支持在**视频缩略图生成**阶段通过 FFmpeg 启用硬件解码加速，默认 `CRYP_FFMPEG_HWACCEL=auto`：

- 自动优先尝试常见硬件后端（`vaapi`、`qsv`、`cuda` 等，取决于本机 FFmpeg 支持）
- 若硬件不可用或驱动不兼容，会自动降级到 CPU 路径
- 兼容 AMD / Intel CPU，以及核显/独显环境（由 FFmpeg 驱动栈决定最终可用后端）

可用环境变量：

- `CRYP_FFMPEG_HWACCEL`：硬件加速策略（默认 `auto`；可设 `none`/`cpu` 强制 CPU，或指定 `vaapi`/`qsv`/`cuda`）

示例（自动探测 + 自动降级）：

```yaml
environment:
  - CRYP_FFMPEG_HWACCEL=auto
```

示例（Intel/AMD iGPU，常见 Linux VAAPI）：

```yaml
environment:
  - CRYP_FFMPEG_HWACCEL=vaapi
```

示例（NVIDIA）：

```yaml
environment:
  - CRYP_FFMPEG_HWACCEL=cuda
```

说明：

- 该加速仅影响 `internal/thumbnail` 的 FFmpeg 缩略图任务。
- 即使显卡不可用，也会自动回退到 CPU，不影响功能可用性。
- `auto` 模式会自动选择可用后端，并自动处理常见输出格式参数。
- 服务启动时会先做一次 GPU 自检；若自检失败，会在当前进程内关闭 GPU 加速并全程使用 CPU。
- 文件内容 AES-GCM 加解密仍走 Go 原生实现（CPU/AES-NI），保持兼容和稳定。

## 技术栈

**后端**: Go 1.24, Gin, SQLite (go-sqlite3, WAL 模式)

**前端**: React, TypeScript, Vite, TailwindCSS v4, Artplayer, react-photo-view

**部署**: Docker 多阶段构建, debian:bookworm-slim

## License

MIT
