# 部署与配置

## Docker Compose

准备 Docker 和 Docker Compose v2，然后执行：

```bash
git clone https://github.com/lori255/cryp.git
cd cryp
cp .env.example .env
docker compose pull
docker compose up -d
```

默认监听宿主机 `9527` 端口，持久化数据位于 `./data`。生产环境应编辑 `.env`，将 `CRYP_DATA_DIR` 指向独立磁盘或受备份保护的目录。

从当前源码构建：

```bash
docker compose up -d --build
```

挂载目录必须允许容器内的 `cryp` 用户读写。遇到 `permission denied` 时，请调整宿主机目录的所有者、组或 ACL，不要把容器改为特权模式。

## 配置

Compose 使用下列宿主机变量：

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `CRYP_PORT` | `9527` | 暴露到宿主机的 HTTP 端口 |
| `CRYP_DATA_DIR` | `./data` | 数据库、保险库配置和加密文件的持久化根目录 |
| `CRYP_IMAGE` | `lori255/cryp:latest` | 要运行的容器镜像 |
| `CRYP_SOURCE_DIR` | `/data` | 容器内允许浏览和导入的根目录 |
| `GOMEMLIMIT` | `256MiB` | Go 运行时内存目标 |
| `CRYP_FFMPEG_HWACCEL` | `auto` | FFmpeg 加速策略 |

容器内应用还支持：

| 变量 | Compose 默认值 | 说明 |
| --- | --- | --- |
| `PORT` | `9527` | 服务监听端口 |
| `DATA_DIR` | `/data/config` | SQLite 和应用配置目录 |
| `VAULT_DIR` | `/data/vaults` | 加密保险库目录 |
| `SOURCE_DIR` | `/data` | 文件浏览和导入边界 |
| `CRYP_FFMPEG_BIN` | `ffmpeg` | 自定义 FFmpeg 可执行文件 |

`SOURCE_DIR` 是安全边界。需要导入其他宿主机目录时，应额外挂载到容器，并把 `CRYP_SOURCE_DIR` 设置为对应的容器路径。不要把宿主机根目录直接暴露给应用。

## GPU 加速

缩略图任务会根据 `CRYP_FFMPEG_HWACCEL` 和当前 FFmpeg/驱动能力选择后端；需要转码的视频播放目前可在 Linux 上使用 VAAPI，否则回退到 CPU。

VAAPI 示例：

```yaml
services:
  cryp:
    devices:
      - /dev/dri/renderD128:/dev/dri/renderD128
    group_add:
      - "<宿主机 render 组 GID>"
    environment:
      CRYP_FFMPEG_HWACCEL: vaapi
```

设备节点和 GID 因发行版而异。GPU 不可用不应阻止服务启动，但转码和缩略图会使用更多 CPU。

## HTTPS 与反向代理

Cryp 默认提供 HTTP。跨主机或互联网访问时，应放在支持 HTTPS 的反向代理后，并限制来源、请求体大小和访问范围。不要直接将未加密的 `9527` 端口暴露到公网。

## 备份与升级

`CRYP_DATA_DIR` 下的数据库、保险库配置和加密文件必须作为一个整体备份。建议在一致性备份前停止容器：

```bash
docker compose stop
# 使用你信任的备份工具复制 CRYP_DATA_DIR
docker compose start
```

升级前先备份，然后执行：

```bash
docker compose pull
docker compose up -d
```

备份只保护文件，不会恢复遗失的保险库密码。请在独立安全位置保存密码，并定期验证恢复流程。

## 常用诊断

```bash
docker compose ps
docker compose logs --tail=200 cryp
docker compose config
```

日志可能包含文件路径和运行环境信息；发布 Issue 前请先删除个人路径、域名、IP 和其他敏感数据。
