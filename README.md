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
docker compose up -d --build
```

访问 `http://localhost:9527`

### docker-compose.yml

```yaml
services:
  cryp:
    build: .
    container_name: cryp
    ports:
      - "9527:9527"
    volumes:
      - /your/data/path:/data
    environment:
      - GOMEMLIMIT=256MiB
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

## 技术栈

**后端**: Go 1.24, Gin, SQLite (go-sqlite3, WAL 模式)

**前端**: React, TypeScript, Vite, TailwindCSS v4, Artplayer, react-photo-view

**部署**: Docker 多阶段构建, debian:bookworm-slim

## License

MIT
