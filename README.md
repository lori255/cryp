# Cryp

Cryp 是一个自托管的加密文件保险库。文件内容和文件名在磁盘上保持加密，登录后可通过 Web 界面上传、导入、浏览图片并流式播放视频。

> Cryp 不提供托管服务。部署者需要自行负责 HTTPS、访问控制、数据备份以及宿主机安全。

## 功能

- AES-256-GCM 分块加密，支持 Range 请求和视频拖动
- AES-SIV 文件名加密，支持长文件名映射
- 浏览器端图片预览和视频播放，无需预先解密完整文件
- 后台上传、目录导入、索引和缩略图任务
- FFmpeg 视频缩略图与按需 HLS 转码
- Docker 部署、SQLite 持久化和移动端/PWA 界面
- 流式 I/O、复用缓冲区和 page-cache 控制，避免内存随文件大小线性增长

## 快速开始

需要 Docker 与 Docker Compose v2：

```bash
git clone https://github.com/lori255/cryp.git
cd cryp
cp .env.example .env
docker compose pull
docker compose up -d
```

访问 `http://localhost:9527`。默认数据保存在仓库目录下的 `data/`；生产部署请在 `.env` 中把 `CRYP_DATA_DIR` 改为独立的持久化目录。

若要从当前源码构建镜像：

```bash
docker compose up -d --build
```

首次打开后，创建保险库并妥善保存密码。没有密码就无法恢复已加密的数据。

## 文档

- [部署、配置、GPU 与备份](docs/DEPLOYMENT.md)
- [开发环境与测试](docs/DEVELOPMENT.md)
- [贡献指南](CONTRIBUTING.md)
- [安全问题报告](SECURITY.md)

## 加密设计

| 组件 | 算法 | 用途 |
| --- | --- | --- |
| 密钥派生 | scrypt | 从密码派生密钥加密密钥 |
| 密钥包装 | AES Key Wrap（RFC 3394） | 存储包装后的主密钥和 MAC 密钥 |
| 文件内容 | AES-256-GCM | 32 KiB 独立分块，支持随机访问 |
| 文件名 | AES-SIV（RFC 5297） | 使用目录上下文保护文件名 |
| 长文件名 | SHA-256 映射 | 处理超出文件系统限制的密文名称 |

Cryp 的加密实现仍应视为应用级安全组件，而不是经过独立密码学审计的通用库。请只从可信来源获取镜像，并为公开访问配置 HTTPS。

## 项目结构

```text
cmd/server/          服务入口与前端嵌入
internal/api/        HTTP API、HLS 与生命周期管理
internal/crypto/     内容、文件名和密钥加密
internal/storage/    SQLite 数据层
internal/task/       后台任务
internal/thumbnail/  FFmpeg/HEIF 缩略图
web/                 React + TypeScript 前端
docs/                面向用户和贡献者的长期文档
```

## 技术栈

- Go 1.24、Gin、SQLite
- React 19、TypeScript、Vite、Tailwind CSS
- FFmpeg、libheif、Artplayer、hls.js
- Docker 多阶段构建

## License

[MIT](LICENSE)
