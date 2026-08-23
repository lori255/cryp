# Cryp 深度代码审核记录

审核日期：2026-08-23
审核基线：`master` / `7f981c8 Improve HLS seek responsiveness`

## 1. 审核范围

本次审核覆盖后端 HTTP 路由与 HLS/FFmpeg 生命周期（`internal/api`）、缩略图队列与硬件加速（`internal/thumbnail`）、会话存储（`internal/session`），以及播放器、API 客户端和文件浏览器（`web/src`）。重点围绕“前几个视频可以播放，随后任意视频均提示转码失败”和“停止播放后 GPU 仍显示 100%”两个现象，沿启动、鉴权、分段读取、停止和回收的完整链路追踪。

### 验证结果与限制

- `go test ./...`：通过。
- `go vet ./...`：通过。
- `go test -race ./...`：通过。
- `npm run build`：通过。
- `npm run lint`：通过。
- 当前审核环境没有 `ffmpeg`/`ffprobe` 可执行文件，只有 `/dev/dri/card0`，没有 `/dev/dri/renderD128`；因此未能进行真实 HLS 编码、VAAPI 和多浏览器集成验证。
- 已补充 HLS pending/停止/请求取消/资产清理屏障、会话快照、缩略图 generation/停止竞态以及加密文件原子替换单元测试；仍缺少真实 FFmpeg、多浏览器和 VAAPI 集成测试。

## 2. 最可能的故障因果链

当前实现允许同一文件在一次慢启动期间启动多个 FFmpeg：

1. `GET /files/hls` 先执行最长 10 秒的 `ffprobe`，随后每个转码 profile 最多等待 5 秒才认为 playlist ready（`internal/api/files.go:397-436`）。
2. 启动调用使用 `context.Background()`，浏览器断开或 hls.js 超时不会取消后端启动（`internal/api/files.go:397`）。
3. hls.js manifest 请求配置了重试；而服务端只有在 ready 后才写入 `s.hls`（`internal/api/files.go:406-408`），pending 启动无法去重。
4. 每个 pending 请求都会增加 `hlsStarts`，并与已登记的 `s.hls` 一起受 `hlsMaxStreams=3` 限制（`internal/api/files.go:348-368`）。达到上限后返回 `429`，前端把包括 429 在内的 fatal 错误统一显示为“视频转码播放失败”。
5. 自然结束的流还会在内存 map 中保留 15 分钟（`internal/api/files.go:640-654`），继续占用 3 个名额；停止请求又无法命中尚未登记的 pending stream（`internal/api/files.go:584-605`）。

因此，“前几个视频正常，之后任意视频失败”与名额耗尽高度吻合；停止后 GPU/FFmpeg 仍显示忙则与 pending 启动未取消、停止后未等待进程退出，以及自然结束/分段仍在写盘的生命周期问题吻合。GPU 工具显示 100% 还可能是驱动/监控的滞后值，当前代码没有实际进程和设备指标可供确认。

## 3. 问题清单

严重级别：**P0** 表示会阻断播放或持续泄漏资源；**P1** 表示高概率造成故障、数据/安全风险或明显错误；**P2** 表示功能退化、维护风险或缺少保护；**P3** 表示低风险改进项。

### T：转码、HLS 与资源生命周期

| 编号 | 级别 | 问题与影响 | 关键位置 |
| --- | --- | --- | --- |
| T-01 | P0 | HLS 名额按 `len(s.hls)+s.hlsStarts` 计数，上限仅 3；达到上限返回 429。前端未区分 429，表现为后续所有视频“转码失败”。 | `internal/api/files.go:348-368` |
| T-02 | P0 | 启动阶段使用 `context.Background()`，请求取消无效；ready 前不写入 `s.hls`，hls.js 超时重试可为同一路径并发启动多个 FFmpeg。 | `internal/api/files.go:397`, `:406-436`; `web/src/components/VideoPlayer.tsx:142-155` |
| T-03 | P0 | stop 只遍历已登记的 `s.hls`；pending stream 尚未入 map，停止播放后无法取消，后台仍可能继续探测/转码并占用名额。 | `internal/api/files.go:397-410`, `:584-605` |
| T-04 | P1 | profile 失败会逐个启动并 kill，单次请求可能经历 10 秒 probe 加 3×5 秒 ready 等待；慢请求与客户端重试叠加，造成启动风暴和错误放大。 | `internal/api/files.go:415-455` |
| T-05 | P0 | FFmpeg 自然结束后固定等待 15 分钟才从 `s.hls` 删除；已结束流仍计入名额，短视频连续播放很快耗尽上限。 | `internal/api/files.go:640-654` |
| T-06 | P1 | 后端只返回通用 500/429 文案，FFmpeg stderr 只写日志，前端将认证、文件不存在、编码器失败、网络超时全部归为“转码失败”，无法诊断或重试。 | `internal/api/files.go:398-403`, `:644-647`; `web/src/components/VideoPlayer.tsx:160-165`, `:237-246` |
| T-07 | P1 | GPU profile 只检查 `h264_vaapi` 和固定 `/dev/dri/renderD128`，没有在 HLS 启动前做实际解码/编码探测；容器未默认映射设备/驱动时会反复失败后才回退 CPU，监控中的 GPU 状态也没有与活动进程关联。 | `internal/api/files.go:781-839`; `Dockerfile:22-24`; `docker-compose.yml:9-14` |
| T-08 | P1 | 缩略图 FFmpeg 与 HLS 共用主机 GPU/FFmpeg 资源，没有并发配额、优先级或统一回收；缩略图任务可与播放抢占设备，放大转码失败和 GPU 峰值。 | `internal/thumbnail/thumbnail.go:69-88`, `:440-500`; `internal/api/files.go:458-502` |
| T-09 | P1 | 停止通过取消 context/kill 直接子进程，但没有显式进程组回收、`Wait` 确认和 shutdown 统一清理；FFmpeg 子进程或驱动任务可能短时残留，造成“停止后 GPU 100%”。 | `internal/api/files.go:487-502`, `:608-655` |
| T-10 | P0 | HLS 内部 content URL 读取 `PORT`，未设置时硬编码 9527；服务实际可由 `-port` 监听 8080，端口不一致会让 ffprobe/ffmpeg 连接失败。 | `internal/api/files.go:377-382`; `cmd/server/main.go:18-32` |
| T-11 | P1 | `-hls_list_size 0` 且没有 `-re`，转码会尽快生成并永久保留全部 `.ts`，长视频/异常流可持续占用临时磁盘；停止清理受进程退出时序影响。 | `internal/api/files.go:475-485`, `:649-655` |
| T-12 | P1 | GPU profile fallback 与其它 profile 复用同一输出目录；失败尝试留下的 playlist/segment 可能被下一次 `waitForHLSReady` 误判为 ready。 | `internal/api/files.go:390-426`, `:532-560` |
| T-13 | P1 | ffprobe 成功时向客户端返回静态 VOD playlist，而 FFmpeg 同时生成自己的 playlist；duration 四舍五入、编码失败或关键帧不齐时，静态清单可能引用不存在/时长不符的 segment。 | `internal/api/files.go:415-421`, `:677-686`, `:732-758` |
| T-14 | P2 | 分段请求等待文件最多 30 秒，停止/删除期间仍可能阻塞请求；缺少对 stream 状态、文件大小稳定性和客户端取消后的统一处理。 | `internal/api/files.go:688-696`, `:761-778` |

### S：安全、会话与缓存

| 编号 | 级别 | 问题与影响 | 关键位置 |
| --- | --- | --- | --- |
| S-01 | P1 | HLS segment 返回 `public, max-age=300`。若反向代理/浏览器缓存未按 vault/session 隔离，明文分段可能跨用户复用或在停止后继续可读。 | `internal/api/files.go:688-695` |
| S-02 | P1 | HLS asset 仅按随机 stream ID 查 map，没有再次校验当前会话的 vault/stream 所属关系；已获得有效登录会话且知道 ID 的用户可能读取另一会话的流。 | `internal/api/files.go:658-675`; 路由 `internal/api/router.go:108-118` |
| S-03 | P1 | `sendBeacon` 成功排队后不检查认证结果，也不能附加 `X-Session-ID`；只有 localStorage session 或跨源 cookie 不可用时，stop 会静默失败，流要等 idle timeout。 | `web/src/lib/api.ts:152-170`; `internal/api/files.go:564-581` |

### R：前端播放器与文件列表

| 编号 | 级别 | 问题与影响 | 关键位置 |
| --- | --- | --- | --- |
| R-01 | P1 | 正常入口始终使用 HLS URL，播放器的 content→HLS fallback 只在 URL 已含 `/files/content` 时触发，实际不可达。 | `web/src/lib/api.ts:144-150`; `web/src/components/FileGridItem.tsx:22-25,110-115`; `web/src/components/VideoPlayer.tsx:237-246` |
| R-02 | P1 | Hls.js fatal、媒体错误和鉴权错误均显示同一句“视频转码播放失败”，没有重试/切换 profile/刷新 session 的入口。 | `web/src/components/VideoPlayer.tsx:160-165`, `:237-246`, `:351-355` |
| R-03 | P2 | 文件列表请求没有 AbortController、请求序号或路径校验；切换目录/排序或触发分页时，旧响应可能晚到并覆盖新列表，造成重复播放入口和错误路径。 | `web/src/pages/FileBrowser.tsx:63-103`, `:108-122` |
| R-04 | P2 | 视频元素/Hls.js 只带 URL，未显式传 `X-Session-ID`，播放依赖同源 cookie；跨源部署或 cookie 过期时会收到 401，但 UI 仍归类为转码失败。 | `web/src/lib/api.ts:65-80`, `:144-184`; `web/src/components/VideoPlayer.tsx:142-165` |

### M：缩略图后台任务

| 编号 | 级别 | 问题与影响 | 关键位置 |
| --- | --- | --- | --- |
| M-01 | P2 | 队列满时静默丢弃任务，无重试、持久化队列或指标；失败冷却期也不向用户暴露，文件可能永久没有缩略图。 | `internal/thumbnail/thumbnail.go:22-30`, `:290-332` |

### Q：测试与可观测性

| 编号 | 级别 | 问题与影响 | 关键位置 |
| --- | --- | --- | --- |
| Q-01 | P1 | 缺少 HLS/API 集成测试、FFmpeg fake、停止竞态测试、并发名额测试、磁盘清理测试和前端播放器错误测试；无活动流/FFmpeg/队列/GPU 实际使用指标，生产故障只能依赖零散日志。 | `internal/api/files.go`; `internal/thumbnail/thumbnail.go`; `web/src/components/VideoPlayer.tsx` |

## 4. 常见 HTTP 结果与可能根因

| HTTP 结果 | 用户表现 | 首要排查项 |
| --- | --- | --- |
| 302 → 200 | 播放正常 | 确认 manifest 和第一个 segment 来自同一 stream 目录 |
| 429 | 统一显示“转码失败” | `hlsStarts`、`len(s.hls)`、pending/自然结束流是否未回收；是否有 hls.js 重试风暴 |
| 500（start） | 首个视频就失败或特定编码失败 | FFmpeg stderr、ffprobe、输入 Range、GPU profile/驱动、内部端口 |
| 401/403 | 播放器仍提示转码失败 | cookie 与 `X-Session-ID`、session 是否过期、跨源凭据策略 |
| 404（manifest/segment） | manifest 能开但黑屏/中途失败 | stream 是否已 stop/被清理、playlist 是否引用旧目录文件、缓存是否命中旧 URL |
| 206/416（content） | 拖动或 ffprobe 失败 | Range 解析、加密块边界、请求取消和上游 FFmpeg seek 行为 |

## 5. 修复优先级

1. **P0：HLS 生命周期和去重**：为 pending/active/stopping 建立按 vault+path 的状态；让启动使用可取消 context；stop 能标记并取消 pending；名额只统计真正活动流；等待 `Wait` 完成后立即释放。
2. **P0：启动可靠性**：统一实际监听端口生成 content URL；每次 profile 使用独立目录并清理旧文件；ready 校验当前进程生成的 playlist/segment；避免静态 playlist 与 FFmpeg playlist 不一致。
3. **P1：资源和错误处理**：进程组/退出确认、临时目录磁盘上限、shutdown 清理；GPU profile 真实 probe 与 HLS/缩略图资源配额；返回可区分的错误码和诊断 ID。
4. **P1：前端恢复**：停止 fatal stream、提供重试和认证提示、修复 content/HLS fallback 与 URL 生命周期；为列表请求加入取消/序列保护。
5. **P2：质量保障**：补充 fake-FFmpeg HLS 集成测试、并发/竞态/清理测试、Range 测试、播放器和文件列表测试，以及活动流/FFmpeg/队列/GPU 指标。

本清单是修复前的基线；每个修复应在真实带音视频的短视频、长视频、无音轨视频、VAAPI 不可用和连续切换播放场景下回归。

## 6. 复核追加问题（2026-08-23）

在开始修复后又对并发、停止和密钥生命周期做了一轮交叉核验，补充以下问题，避免只记录最初的名额耗尽链路：

| 编号 | 级别 | 追加问题与影响 | 处理状态 |
| --- | --- | --- | --- |
| T-15 | P1 | 缩略图队列保存会话密钥的原始切片；登出/过期清零时，尚未执行的任务会随机解密失败，并可能发生数据竞态。 | 已修复：入队复制密钥，任务结束清零副本。 |
| T-16 | P1 | stop 只发取消信号而不等待 FFmpeg 退出；连续 stop/start 或删除 vault 时旧进程会短时继续占用 GPU、磁盘和文件句柄。 | 已修复：记录完成信号，停止接口和破坏性操作等待有限窗口；FFmpeg 使用独立进程组回收。 |
| T-17 | P1 | 自然结束的 VOD 为保证分片可读会保留一段时间；同路径的新请求可能复用已结束的旧转码，文件被替换后会播放旧内容。 | 已修复：已完成流只做分片保留，不参与新的同路径启动复用。 |
| T-18 | P1 | 分片/playlist 尚未出现时的等待没有监听 stop，停止期间请求仍可阻塞至超时。 | 已修复：等待同时监听 stream stop 信号并在返回前检查状态。 |
| S-04 | P1 | HLS asset 仅校验 vault，知道 stream ID 的同 vault 会话可读取其他会话的明文分片；stop 也可被跨会话滥用。 | 已修复：stream 维护请求会话所有者集合，asset/stop 按所有者校验。 |
| S-05 | P1 | Session.Store.Get 返回内部 Session 指针，logout/清理清零密钥时会与请求、FFmpeg HTTP handler 并发读写。 | 已修复：Create/Get 使用存储所有权和不可变快照，请求不再持有 store 内部切片。 |
| M-02 | P2 | 缩略图扫描停止时仅取消外部命令，递归扫描大型 vault 仍可能继续很久，拖慢 shutdown。 | 已缓解：扫描递归增加 context 检查；底层单次目录读取仍受文件系统调用时延影响。 |
| M-03 | P1 | 同一路径文件被上传/导入替换时，旧缩略图或仍在运行的旧生成任务可能覆盖新文件的预览。 | 已修复：替换后失效缓存，队列使用 generation 令牌并在原子提交前校验。 |
| T-19 | P1 | stop/删除 vault 只等待固定 5 秒且忽略超时结果；若 FFmpeg 或其子进程未及时退出，破坏性操作仍会继续，可能留下 GPU、文件句柄或正在读取已删除文件的进程。 | 已修复：等待函数返回完成状态；vault/文件破坏性操作在超时返回 504，后续重试仍会等待正在停止的目标。 |
| T-20 | P2 | stop 接口即使等待被请求取消或达到超时，仍返回 200 和 `stopped`，客户端无法判断资源是否仍在回收。 | 已修复：完成返回 200，超时返回 202、`stopping` 和 `complete:false`。 |
| R-05 | P2 | 普通 `/files/content` 播放遇到 401/403/404/服务器错误时，原生 `video:error` 无法取得状态，直接回退 HLS，最终把鉴权/文件问题显示成转码失败。 | 已修复：回退前用 Range 探测状态，区分鉴权、文件和服务错误；只有状态可用时才启动 HLS。 |
| M-04 | P1 | 缩略图文件替换后，旧 generation 任务完成时会无条件删除同路径的 `queued` 标记，可能让新 generation 被重复入队或状态被旧任务覆盖。 | 已修复：完成/失败状态仅在 generation 仍匹配时更新，并用独立提交锁保护原子替换。 |
| T-21 | P1 | 后台导入覆盖同路径文件前没有停止该路径的 HLS；旧 FFmpeg 可能在密文被截断/替换期间继续读取，产生混合分片并放大后续转码失败。 | 已修复：导入逐文件替换前获取生命周期 lease，停止同路径 HLS 并在写入完成前阻止新 start。 |
| M-05 | P2 | 空加密文件会被当作缩略图生成成功但没有输出，后续每次请求都可能重新入队。 | 已修复：空文件进入失败冷却，不再立即重复排队。 |
| T-22 | P1 | pending 启动已被 stop 标记或已超时但尚未从 map 移除时，新播放请求会无条件加入该 pending，最终收到 `context.Canceled`/deadline 错误；快速 stop/start 同一路径因此再次显示“转码失败”。 | 已修复：新请求等待旧 pending 完成并重新查找，不再复用已取消或过期的启动。 |
| T-23 | P0 | 主程序直接使用 `router.Run`，没有优雅关闭流程；服务退出时独立 FFmpeg 进程组可能成为 orphan，继续占用 GPU/CPU、临时目录和文件句柄，重启后表现为 GPU 长时间 100%。 | 已修复：增加 HTTP graceful shutdown，并发停止/等待全部 HLS 后再关闭任务/缩略图/会话资源。 |
| T-24 | P2 | FFmpeg 编码器探测使用 `sync.Once`，一次超时或暂时不可执行会永久缓存空结果；运行环境随后恢复也不会重新启用 VAAPI，导致持续 CPU 转码和错误的 GPU 使用判断。 | 已修复：成功结果短 TTL 缓存，失败结果不缓存并串行重试。 |
| T-25 | P2 | HLS owner 以 `sessionID` 集合去重，同一会话打开两个同路径播放器时，一个播放器关闭会删除唯一 owner 并停止另一播放器仍在使用的 stream。 | 已缓解：维护同会话引用计数；协议级 opaque lease/token 仍可作为后续增强。 |
| M-06 | P1 | 缩略图旧 generation 任务在空文件路径返回 `skipped` 时，无条件写入失败冷却；若新文件/新任务已排队，会被旧任务错误地抑制 5 分钟。 | 已修复：仅当前 generation 才能写入 `failed` 状态，并增加替换竞态测试。 |
| M-07 | P2 | HEIF 缩略图在 `heif-convert` 前完整解密到临时明文文件，没有独立读取超时或大小上限；异常/超大输入可能长期占用磁盘并拖慢单 worker。 | 已缓解：解密和转换输入/输出增加 512 MiB 大小预算并沿用可取消 context；底层阻塞 I/O 仍受文件系统时延影响。 |
| M-08 | P2 | 缩略图硬件探测只在 Generator 启动时执行；驱动/设备在运行中恢复或变化不会重新探测，任务只能逐个失败后回退 CPU。 | 部分缓解：单任务增加总时限并在失败后回退 CPU；运行中设备恢复的动态重探测仍待后续。 |
| T-26 | P1 | 删除目录/批量删除目录时只停止目录自身的 HLS；递归删除子视频前没有停止子路径 stream，FFmpeg 可能继续读取被删除/替换的密文并产生失败分片。 | 已修复：按路径前缀收集并等待所有后代 stream。 |
| T-27 | P1 | 上传、导入或删除在停止旧 stream 后释放锁再写文件；窗口内新的播放请求可以重新启动同路径 FFmpeg，随后与覆盖/删除并发读取同一密文。 | 已修复：HLS 生命周期读写屏障覆盖上传、删除和导入写入窗口。 |
| T-28 | P1 | logout 停止 owner 与删除 session 之间没有生命周期屏障；同一会话可在窗口内重新启动 HLS，随后内部认证 session 被删除，表现为转码/分片失败。 | 已修复：logout 与 HLS start/stop 串行化后再删除会话。 |
| T-29 | P1 | 上传和导入使用 `os.Create` 直接截断目标密文；即使 HLS 已停止，仍在进行的普通 `/files/content` 读取会看到截断/混合内容，触发 seek 或转码失败。 | 已修复：同目录临时文件 fsync 后原子替换目标，失败不破坏旧文件。 |
| S-06 | P1 | 缩略图响应使用 `public, max-age=86400`，普通 content 也未声明私有缓存；共享代理可能把一个会话的明文缩略图/媒体响应提供给另一个会话。 | 已修复：受保护媒体使用 `private, no-store` 并按 Origin、Cookie、`X-Session-ID` 分隔。 |
| S-07 | P1 | HLS start 的 302 重定向未声明不可缓存；共享代理可能缓存同一路径的 stream ID，使后续会话复用旧转码或收到 404/权限错误。 | 已修复：start、stop 和 asset 响应使用 `no-store`/私有缓存并按 Origin、Cookie、`X-Session-ID` 分隔。 |
| T-30 | P1 | 首个 HLS 请求的启动 context 脱离 HTTP 请求；浏览器在 FFmpeg ready 前断开且 stop 请求未到达时，pending 和 FFmpeg 仍可运行最长 30 秒，占用名额/GPU 并放大后续“转码失败”。 | 已修复：启动改为后台 pending 任务；请求取消按 owner 引用释放，最后一个 owner 消失时取消 pending，仍有共享等待者时不误伤。 |
| T-31 | P2 | playlist/segment 文件通过存在性等待后再调用 `c.File`，期间 cleanup 可能删除 stream 目录，形成 TOCTOU，导致偶发 404 或不完整分片响应。 | 已修复：asset 读取持有 `assetMu` 读锁，目录清理取得写锁后再删除，避免等待与打开之间的竞态。 |
| T-32 | P2 | 启动取消路径的 `stopAndWaitHLS` 与 cleanup goroutine 可能同时调用同一 `exec.Cmd.Wait`；重复 Wait 会丢失真实退出错误并导致不稳定的测试/回收行为。 | 已修复：`hlsStream` 使用 `sync.Once` 统一等待并复用退出错误。 |

以上追加项与第 3 节原始编号互补；真实 FFmpeg、VAAPI 驱动和浏览器集成回归仍受当前环境缺少 `ffmpeg`/`ffprobe` 及 render node 的限制。

## 7. 结构与质量优化（2026-08-23）

在完成上述 HLS 生命周期修复后，又按“避免继续堆补丁、收敛边界”的原则落地了一轮低风险优化：

| 编号 | 优化 | 落地点与收益 |
| --- | --- | --- |
| O-01 | 导入源沙箱 | 新增 `internal/pathguard`，浏览和导入共用同一个真实路径校验；解析 symlink、拒绝越界和导入树中的 symlink，并禁止 `deleteSource` 指向根目录或应用保留目录（`DATA_DIR`/`VAULT_DIR`）。`SOURCE_DIR`/`BROWSE_ROOT` 可配置。 |
| O-02 | 后台任务所有权 | `task.Manager` 使用 `context.Context`、任务完成信号和 `WaitGroup`；取消不再提前释放独占名额，新增 vault 级 quiesce 和服务 shutdown 等待，避免任务访问已关闭 DB 或已删除 vault。 |
| O-03 | 密钥生命周期 | 请求、导入、索引、登录和缩略图使用明确的 clone/owner，并统一通过 `VaultKeys.Zero` 清理副本。 |
| O-04 | 加密读取健壮性 | `DecryptingReader` 对截断/异常 chunk 返回 `ErrCorruptContent`，不再在短密文上 slice panic；异常密文尺寸也会被钳制。导入加密支持 context 取消。 |
| O-05 | 前端错误与轮询边界 | API 客户端引入 `ApiError(status/code/details)`，正确处理 204/空响应和任意 `Headers`；任务轮询改为请求完成后排程并区分轮询错误；播放器 probe 请求随组件销毁取消。 |
| O-06 | 小步结构化 | 将纯 HLS HTTP 错误映射抽到 `hls_errors.go` 并补单元测试，作为后续拆分 HLS Manager 的安全切入点。 |

本轮验证：`go test ./...`、`go test -race ./...`、`go vet ./...`、`git diff --check`、`cd web && npm run lint`、`cd web && npm run build` 均通过。

仍需后续处理的架构项：`internal/api/files.go` 仍是大型单体（HTTP、HLS、传输和索引职责混合），尚未建立真实 FFmpeg fake/API 集成测试与活动流/队列/GPU metrics；跨 DB 与磁盘的 vault 删除也仍属于补偿式语义，生产部署应配合 orphan sweeper 和失败重试告警。当前环境仍没有可执行的 FFmpeg/VAAPI render node，因此不能把这些验证缺口误判为已解决。

## 8. 继续优化复核（2026-08-23）

对上一轮改动做生命周期和可维护性复核后，新增问题与处理状态如下：

| 编号 | 级别 | 问题与影响 | 处理状态 |
| --- | --- | --- | --- |
| M-09 | P1 | vault 删除只等待导入/索引和 HLS；缩略图 FFmpeg 或扫描仍可能在 `RemoveAll` 后读 vault、重建 `thumbnails` 目录，且队列密钥继续驻留内存。 | 已修复：Generator 增加按 vault 的 quiesce/cancel/wait；队列在 claim 阶段再次闸门校验，删除成功后以 tombstone/`ForgetVault` 收敛。 |
| S-08 | P1 | 删除接口直接信任数据库中的 `vault.Path`；数据库异常或配置漂移可能使 `deleteFiles=true` 删除 vault 根目录或任意路径。 | 已修复：`pathguard.ValidateVaultPath` 要求路径是配置 vault 根下、basename 与 ID 一致、非 symlink 且真实路径不越界；失败则 fail-closed。 |
| S-09 | P2 | 导入源经过路径树校验后，打开阶段仍有 Lstat/真实文件被替换的 TOCTOU 窗口，可能把校验对象与实际读取对象分离。 | 已缓解：加密入口打开后再次 `Lstat`，用 `os.SameFile` 和 regular-file 检查校验 FD 身份；完整 `openat2`/目录 FD 沙箱仍是 Linux 专项后续项。 |
| Q-02 | P2 | 任务 API 在 manager 为空、取消竞态、pending 删除和内部错误回传时行为不一致，可能 panic、覆盖已完成状态或泄露实现细节。 | 已修复：统一 503/code；取消失败后重新读取状态且不覆写；pending 与 running 均禁止删除；提取 vault ownership helper；客户端只收到稳定错误码。 |
| Q-03 | P2 | HLS/缩略图硬编码 `ffmpeg`，难以固定版本、隔离测试和做真实进程回收回归。 | 已改善：支持 `CRYP_FFMPEG_BIN`；补充 fake-FFmpeg 启动、playlist ready、单一 `Wait` 和进程组停止测试。 |

本轮新增验证：`go test ./...`、`go test -race ./...`、`go vet ./...`、前端 lint/build 已重新执行并通过。真实硬件驱动、跨浏览器和跨 DB/磁盘补偿仍不应以单元测试通过替代生产观测；建议后续继续拆出 HLS Manager，并增加活动流、FFmpeg profile/退出码、缩略图队列及 GPU/磁盘 metrics。

## 9. 架构与边界复核（2026-08-23）

本轮在提交前又做了一次“是否继续堆补丁”的复核，落地项和明确保留的架构债务如下：

| 编号 | 级别 | 结论与处理状态 |
| --- | --- | --- |
| S-10 | P1 | vault 删除从“校验后直接 `RemoveAll`”改为 `QuarantineVaultPath` 原子移到 vault 根下的唯一 sibling，再删除隔离路径；临时 reservation 在失败时自动清理。该方案收窄了检查-删除窗口，但对能写入整个 vault 根的高权限外部进程仍不能替代 Linux `openat2`/目录 FD 沙箱。 |
| T-33 | P2 | HLS 并发上限响应补充稳定 `code=hls_capacity_exceeded`，与启动超时/取消/失败契约一致；前端可将 429 区分为资源繁忙而不是泛化为转码失败。 |
| Q-04 | P1 | `Server.Shutdown` 现在先取得 context-aware 的 HLS 生命周期写屏障，等待上传/导入替换/删除操作退出；task manager 增加 `Wait`，有界 shutdown 超时后仍等待 worker 完成，再关闭 DB/session，避免 use-after-close 和 orphan FFmpeg。 |
| M-10 | P2 | 缩略图 vault tombstone 完成回收时同步清理 `queued`/`failed`/`generations` 的 per-file bookkeeping，避免删除大量 vault 后内存只增不减。 |
| Q-05 | P2 | 上传任务进度更新统一经过 manager 读锁生命周期屏障，并在上传入口校验 task 所属 vault；无 task manager 时返回 503，不再因带 taskId 的请求 panic。 |
| A-01 | P2 | `internal/api/files.go` 仍是约 2600 行的多职责单体（文件传输、HLS、上传、索引、缩略图 HTTP）。本轮只抽离纯错误映射，暂不做机械拆文件，避免在无真实 FFmpeg/浏览器回归时扩大行为变更；后续应按职责拆成 `files_hls`/`files_content`/`files_mutation` 等模块。 |
| A-02 | P2 | HLS 与 thumbnail 各自维护 FFmpeg binary、硬件探测和 profile 模型。本轮统一 `CRYP_FFMPEG_BIN`、修正 HLS encoder cache 按 binary 隔离，并为失败的 HLS 硬件 profile 增加短 TTL 冷却；共享 `internal/ffmpeg` 能力层仍属于下一阶段跨硬件矩阵重构。 |
| A-03 | P2 | vault 删除仍是 DB 与磁盘的补偿式事务：文件已隔离/删除后 DB 操作可能失败。当前保持 tombstone/quiesce 以便重试；生产部署应增加 orphan quarantine sweeper、失败告警和可观测指标。 |
| Q-06 | P1 | 任务启动在预约独占名额后、写入 durable task/加入 `running` 与 `WaitGroup` 前存在 admission 窗口；vault 删除或服务关停可能误判“没有运行任务”，并发删除任务行或关闭 DB。 | 已修复：`task.Manager` 增加 admission 读写屏障，将任务行创建、`running` 注册和 `WaitGroup.Add` 放入同一不可被 quiesce/shutdown 穿透的临界区；耗时源校验/文件计数若在屏障外完成，进入 admission 时会再次检查 closed/quiescing 并 fail-closed。 |
| Q-07 | P1 | `StartImport`/`StartRebuildIndex` 在源校验、文件计数和 durable admission 之间仍可能长时间悬挂；若只等待 `running` worker，vault 删除或 shutdown 会提前返回并与启动者交叉。 | 已修复：用 `pendingStarts` + `startWg` 覆盖整个 public `Start*` 调用；`QuiesceVault`、`ForgetVault`、`ResumeVault` 和 `Shutdown` 均等待或保留 tombstone，直到最后一个启动者退出。`Wait` 只允许在 `Shutdown` 已建立 admission 边界后调用。 |
| M-11 | P2 | 缩略图硬件探测把“生成测试文件”和“解码测试文件”共用一个 10 秒 context；冷启动驱动耗尽前半段预算后，第二次探测会被误判为不可用，导致无谓 CPU 回退。 | 已修复：两个探测命令使用独立超时预算，并补充探测生命周期回归覆盖。 |
| T-34 | P2 | 已知不可用的 VAAPI profile 每次播放都会重新启动并等待 readiness 后才回退 CPU，连续播放时会制造短时 GPU 峰值和启动延迟。 | 已缓解：硬件 profile 启动失败进入按 binary/profile/设备参数隔离的 30 秒冷却；CPU fallback 不受冷却影响，成功后立即清除记录。真实驱动恢复仍需硬件集成测试。 |
| T-35 | P2 | 前端 stop 请求忽略 202/网络失败，页面切换后无法再次确认进程组是否已完成退出；虽然后端会继续清理，但 UI 与回收状态脱节。 | 已缓解：`stopHls` 对 202 或一次网络失败按 `Retry-After` 最多重试一次，仍保持卸载场景的 best-effort 语义。 |
| Q-08 | P2 | `task.Manager` 的缩略图 enqueuer setter 与导入 worker 直接读字段，运行时重配置会发生 data race；初始化阶段通常不触发，但接口没有明确不可变约束。 | 已修复：setter 加锁，worker 通过同步快照读取 callback；不改变初始化和循环依赖解法。 |
| S-11 | P1 | 任务查询接口直接序列化 `storage.TaskRecord`，会把绝对源路径和导入错误中的主机/系统细节返回给同 vault 客户端。 | 已修复：引入 API 专用 task DTO；源路径只返回 source root 下的相对路径（越界旧记录退化为 basename），错误统一为稳定用户文案，原始诊断仅保留服务端日志。 |
| Q-09 | P2 | 列表、上传路径解析、建目录、建 vault、multipart 和 SPA fallback 将底层错误字符串直接拼进响应，导致实现细节泄露且客户端无法依赖稳定错误码。 | 已修复：响应固定文案并增加稳定 code，详细错误改为结构化日志；任务查询能区分 `sql.ErrNoRows` 与数据库故障。 |
| A-04 | P2 | browse-dir 为保持现有导入协议返回解析后的主机绝对路径，部署目录结构会暴露给已登录客户端。 | 保留兼容行为并记录为后续协议改造：需要同时引入 source-root-relative path token 和前端导航迁移，不能只改响应字段。 |

本轮新增验证目标：除 Go 全量与 race/vet、前端 lint/build 外，还应在具备真实 FFmpeg、VAAPI render node 和带音视频样本的环境回归连续播放/停止/切换、GPU 进程退出、磁盘清理及 vault 删除失败重试。单元 fake-FFmpeg 测试只覆盖基础 ready/stop/Wait 契约，不能代替上述硬件和浏览器验证。

## 10. 继续优化审查（2026-08-23）

对修复后的实现做了新一轮故障路径和架构复核，新增以下项目；本节先于代码变更提交，后续修复提交会逐项更新状态：

| 编号 | 级别 | 问题与影响 | 关键位置 | 计划 |
| --- | --- | --- | --- | --- |
| T-36 | P1 | `waitForHLSReady` 通过 `cmd.ProcessState` 判断 FFmpeg 是否退出，但该字段只有 `Wait` 执行后才会填充；FFmpeg 立即失败时会完整等待 5 秒，硬件 profile 失败再叠加 CPU fallback，放大启动延迟、pending 占用和客户端重试风暴。 | `internal/api/files.go:908-938` | 引入单一进程退出通知，ready 等待立即感知退出，并与统一 `Wait` 结果复用。 |
| Q-10 | P2 | 加密 Range/seek 是 FFmpeg content URL 的关键依赖，但缺少 206、416、负值、多 Range、空文件和请求取消的直接回归测试；边界回归只能靠间接 HLS 测试发现。 | `internal/api/files.go:1785-2007` | 补充 focused Range 测试，锁定协议和资源释放契约。 |
| Q-11 | P2 | `task.Manager` 的可选 guard setter/getter nil receiver 行为不一致；上传任务还绕过 `Start*` 的 task ID/计数约束，并且 DB 错误包装不统一，增加嵌入式调用 panic 和诊断分支。 | `internal/task/manager.go:131-174,486-517` | 统一 nil 安全和输入校验，补单元测试并保持现有 API 兼容。 |
| A-05 | P2 | `internal/api/files.go` 仍是约 2.7k 行单体，混合普通文件传输、HLS、上传、删除、索引和缩略图 HTTP；继续在同一文件堆叠生命周期补丁会增加冲突和回归概率。 | `internal/api/files.go` | 先按同 package、无行为变化的边界拆出 content/download 与 HLS 文件，再评估跨 package FFmpeg 能力层。 |
| A-06 | P2 | HLS 与 thumbnail 各自维护 FFmpeg binary、硬件探测和 profile 模型；直接抽共享包可能改变解码/编码语义，当前缺少真实硬件矩阵，暂不贸然合并。 | `internal/api/files.go`, `internal/thumbnail/thumbnail.go` | 本轮只统一生命周期/测试契约；共享 `internal/ffmpeg` 延后至有硬件集成测试后。 |

本节的 T-36、Q-10、Q-11 为本轮优先修复项；A-05 采用机械拆分并逐步验证，A-06 保留为明确的后续架构债务。
