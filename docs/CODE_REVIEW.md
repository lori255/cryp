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
- `npm run lint`：失败；`web/src/components/VideoPlayer.tsx:23` 存在 effect 内同步 `setState`，并伴有 hook 警告。
- 当前审核环境没有 `ffmpeg`/`ffprobe` 可执行文件，只有 `/dev/dri/card0`，没有 `/dev/dri/renderD128`；因此未能进行真实 HLS 编码、VAAPI 和多浏览器集成验证。
- 除加密包外，后端 HLS/API、FFmpeg 回收和前端播放器缺少自动化测试，以下结论主要由代码路径、时序和配置推导，并应在修复后用真实媒体回归。

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
| M-07 | P2 | HEIF 缩略图在 `heif-convert` 前完整解密到临时明文文件，没有独立读取超时或大小上限；异常/超大输入可能长期占用磁盘并拖慢单 worker。 | 待修复：为解密阶段增加可取消的 I/O/大小预算，并在超限时清理。 |
| M-08 | P2 | 缩略图硬件探测只在 Generator 启动时执行；驱动/设备在运行中恢复或变化不会重新探测，任务只能逐个失败后回退 CPU。 | 待修复：对探测失败或设备变化采用可重试的 TTL/健康状态。 |
| T-26 | P1 | 删除目录/批量删除目录时只停止目录自身的 HLS；递归删除子视频前没有停止子路径 stream，FFmpeg 可能继续读取被删除/替换的密文并产生失败分片。 | 已修复：按路径前缀收集并等待所有后代 stream。 |
| T-27 | P1 | 上传、导入或删除在停止旧 stream 后释放锁再写文件；窗口内新的播放请求可以重新启动同路径 FFmpeg，随后与覆盖/删除并发读取同一密文。 | 已修复：HLS 生命周期读写屏障覆盖上传、删除和导入写入窗口。 |
| T-28 | P1 | logout 停止 owner 与删除 session 之间没有生命周期屏障；同一会话可在窗口内重新启动 HLS，随后内部认证 session 被删除，表现为转码/分片失败。 | 已修复：logout 与 HLS start/stop 串行化后再删除会话。 |
| T-29 | P1 | 上传和导入使用 `os.Create` 直接截断目标密文；即使 HLS 已停止，仍在进行的普通 `/files/content` 读取会看到截断/混合内容，触发 seek 或转码失败。 | 已修复：同目录临时文件 fsync 后原子替换目标，失败不破坏旧文件。 |
| S-06 | P1 | 缩略图响应使用 `public, max-age=86400`，普通 content 也未声明私有缓存；共享代理可能把一个会话的明文缩略图/媒体响应提供给另一个会话。 | 已修复：受保护媒体使用 `private, no-store` 并按 Cookie、`X-Session-ID` 分隔。 |

以上追加项与第 3 节原始编号互补；真实 FFmpeg、VAAPI 驱动和浏览器集成回归仍受当前环境缺少 `ffmpeg`/`ffprobe` 及 render node 的限制。
