import { useEffect, useMemo, useState } from 'react'
import { PhotoProvider, PhotoView } from 'react-photo-view'
import { X, CopyMinus, Loader2, RefreshCw, Trash2, ScanSearch, Image as ImageIcon, Film, File as FileIcon, ShieldCheck, ChevronDown, ChevronRight } from 'lucide-react'
import VideoPlayer from './VideoPlayer'
import { api, type DuplicateGroup, type DuplicateFileItem, type DuplicateStats, formatDate, formatSize, isImage, isVideo } from '../lib/api'
import { useImagePreviewSrc } from '../lib/useImagePreviewSrc'

interface DuplicatePanelProps {
  vaultId: string
  open: boolean
  onClose: () => void
  onRefresh?: () => void
  onOpenTasks?: () => void
}

type SelectionMap = Record<string, boolean>
type KeeperMap = Record<string, string>
type ExpandedMap = Record<string, boolean>
const DUPLICATE_PAGE_SIZE = 20

function buildDefaultState(groups: DuplicateGroup[]): { selections: SelectionMap; keepers: KeeperMap } {
  const selections: SelectionMap = {}
  const keepers: KeeperMap = {}

  for (const group of groups) {
    if (group.files.length === 0) continue
    const sorted = [...group.files].sort((a, b) => {
      if (a.modTime !== b.modTime) return a.modTime - b.modTime
      return a.path.localeCompare(b.path, 'zh-CN', { numeric: true, sensitivity: 'base' })
    })
    const keeper = sorted[0].path
    keepers[group.contentHash] = keeper
    for (const file of group.files) {
      selections[file.path] = file.path !== keeper
    }
  }

  return { selections, keepers }
}

export default function DuplicatePanel({ vaultId, open, onClose, onRefresh, onOpenTasks }: DuplicatePanelProps) {
  const [groups, setGroups] = useState<DuplicateGroup[]>([])
  const [loading, setLoading] = useState(false)
  const [loadingMore, setLoadingMore] = useState(false)
  const [rebuilding, setRebuilding] = useState(false)
  const [deleting, setDeleting] = useState(false)
  const [error, setError] = useState('')
  const [message, setMessage] = useState('')
  const [indexRequired, setIndexRequired] = useState(false)
  const [selections, setSelections] = useState<SelectionMap>({})
  const [keepers, setKeepers] = useState<KeeperMap>({})
  const [expanded, setExpanded] = useState<ExpandedMap>({})
  const [stats, setStats] = useState<DuplicateStats>({
    groupCount: 0,
    fileCount: 0,
    duplicateFileCount: 0,
    totalBytes: 0,
    duplicateTotalBytes: 0,
  })
  const [hasMore, setHasMore] = useState(false)
  const [nextOffset, setNextOffset] = useState(0)
  const [activeVideo, setActiveVideo] = useState<{ url: string; title: string } | null>(null)

  useEffect(() => {
    if (!open) return
    void loadGroups()
  }, [open, vaultId])

  async function loadGroups() {
    setLoading(true)
    setError('')
    try {
      const data = await api.listDuplicates(vaultId, 0, DUPLICATE_PAGE_SIZE)
      const nextGroups = data.groups || []
      const defaults = buildDefaultState(nextGroups)
      setIndexRequired(Boolean(data.indexRequired))
      setGroups(nextGroups)
      setSelections(defaults.selections)
      setKeepers(defaults.keepers)
      setExpanded({})
      setStats(data.stats ?? {
        groupCount: 0,
        fileCount: 0,
        duplicateFileCount: 0,
        totalBytes: 0,
        duplicateTotalBytes: 0,
      })
      setHasMore(Boolean(data.hasMore))
      setNextOffset(data.nextOffset ?? nextGroups.length)
    } catch (err) {
      setError(err instanceof Error ? err.message : '查重结果加载失败')
    } finally {
      setLoading(false)
    }
  }

  async function loadMoreGroups() {
    if (loadingMore || loading || !hasMore) return
    setLoadingMore(true)
    setError('')
    try {
      const data = await api.listDuplicates(vaultId, nextOffset, DUPLICATE_PAGE_SIZE)
      const appendedGroups = data.groups || []
      const defaults = buildDefaultState(appendedGroups)
      setIndexRequired(Boolean(data.indexRequired))
      setGroups((prev) => [...prev, ...appendedGroups])
      setSelections((prev) => ({ ...prev, ...defaults.selections }))
      setKeepers((prev) => ({ ...prev, ...defaults.keepers }))
      setStats(data.stats ?? stats)
      setHasMore(Boolean(data.hasMore))
      setNextOffset(data.nextOffset ?? nextOffset + appendedGroups.length)
    } catch (err) {
      setError(err instanceof Error ? err.message : '加载更多重复文件失败')
    } finally {
      setLoadingMore(false)
    }
  }

  async function handleRebuild() {
    setRebuilding(true)
    setError('')
    setMessage('')
    try {
      await api.rebuildFileIndex(vaultId)
      setMessage('重建索引任务已加入任务列表')
      onOpenTasks?.()
    } catch (err) {
      setError(err instanceof Error ? err.message : '重建索引失败')
    } finally {
      setRebuilding(false)
    }
  }

  async function handleDeleteSelected() {
    const paths = Object.entries(selections).filter(([, selected]) => selected).map(([path]) => path)
    if (paths.length === 0) return
    if (!confirm(`确定删除选中的 ${paths.length} 个重复文件吗？`)) return

    setDeleting(true)
    setError('')
    setMessage('')
    try {
      const data = await api.deleteFilesBulk(vaultId, paths)
      const failedCount = Object.keys(data.failed || {}).length
      setMessage(failedCount > 0
        ? `已删除 ${data.deleted.length} 个文件，${failedCount} 个删除失败`
        : `已删除 ${data.deleted.length} 个重复文件`)
      await loadGroups()
      onRefresh?.()
    } catch (err) {
      setError(err instanceof Error ? err.message : '批量删除失败')
    } finally {
      setDeleting(false)
    }
  }

  function selectGroupDuplicates(group: DuplicateGroup) {
    const keeperPath = keepers[group.contentHash]
    setSelections((prev) => {
      const next = { ...prev }
      for (const file of group.files) {
        next[file.path] = file.path !== keeperPath
      }
      return next
    })
  }

  function selectAllDuplicates() {
    setSelections((prev) => {
      const next = { ...prev }
      for (const group of groups) {
        const keeperPath = keepers[group.contentHash]
        for (const file of group.files) {
          next[file.path] = file.path !== keeperPath
        }
      }
      return next
    })
  }

  function setKeeper(group: DuplicateGroup, path: string) {
    setKeepers((prev) => ({ ...prev, [group.contentHash]: path }))
    setSelections((prev) => {
      const next = { ...prev }
      for (const file of group.files) {
        next[file.path] = file.path !== path
      }
      return next
    })
  }

  const selectedSummary = useMemo(() => {
    let count = 0
    let bytes = 0
    for (const group of groups) {
      for (const file of group.files) {
        if (selections[file.path]) {
          count++
          bytes += file.size
        }
      }
    }
    return { count, bytes }
  }, [groups, selections])

  function toggleExpanded(hash: string) {
    setExpanded((prev) => ({ ...prev, [hash]: !prev[hash] }))
  }

  if (!open) return null

  return (
    <>
      <div className="fixed inset-0 bg-black/40 z-40" onClick={onClose} />
      <div className="fixed right-0 top-0 h-full w-full max-w-5xl bg-gray-950 border-l border-gray-800 z-50 flex flex-col">
        <div className="flex items-center justify-between px-4 h-14 border-b border-gray-800 flex-shrink-0">
          <div className="flex items-center gap-2">
            <CopyMinus className="w-5 h-5 text-amber-400" />
            <h2 className="text-lg font-semibold text-white">重复文件</h2>
            {(loading || loadingMore || rebuilding || deleting) && <Loader2 className="w-4 h-4 text-blue-400 animate-spin" />}
          </div>
          <div className="flex items-center gap-2">
            <button
              onClick={() => void loadGroups()}
              className="inline-flex items-center gap-1 px-3 py-1.5 text-xs rounded-lg border border-gray-800 text-gray-300 hover:border-gray-700"
            >
              <RefreshCw className="w-3.5 h-3.5" /> 刷新
            </button>
            <button
              onClick={handleRebuild}
              disabled={rebuilding || loading || deleting}
              className="inline-flex items-center gap-1 px-3 py-1.5 text-xs rounded-lg border border-gray-800 text-gray-300 hover:border-gray-700 disabled:opacity-50"
            >
              <ScanSearch className="w-3.5 h-3.5" /> 重建索引
            </button>
            <button onClick={onClose} className="p-1 text-gray-400 hover:text-white">
              <X className="w-5 h-5" />
            </button>
          </div>
        </div>

        <div className="px-4 py-3 border-b border-gray-800 flex flex-wrap items-center justify-between gap-3 flex-shrink-0">
          <div className="text-sm text-gray-400">
            已选 {selectedSummary.count} / 总重复 {stats.duplicateFileCount} 个文件，可释放 {formatSize(selectedSummary.bytes)} / {formatSize(stats.duplicateTotalBytes)}
            <div className="text-xs text-gray-500 mt-1">
              共 {stats.groupCount} 个重复组，涉及 {stats.fileCount} 个文件
            </div>
          </div>
          <div className="flex items-center gap-2">
            <button
              onClick={selectAllDuplicates}
              disabled={groups.length === 0 || loading || loadingMore}
              className="inline-flex items-center gap-1 px-3 py-2 text-sm rounded-lg border border-gray-800 text-gray-300 hover:border-gray-700 disabled:opacity-50"
            >
              <ShieldCheck className="w-4 h-4" /> 一键选择重复文件
            </button>
            <button
              onClick={handleDeleteSelected}
              disabled={selectedSummary.count === 0 || deleting || loading || loadingMore}
              className="inline-flex items-center gap-1 px-3 py-2 text-sm rounded-lg bg-red-500/15 text-red-300 hover:bg-red-500/20 disabled:opacity-50"
            >
              <Trash2 className="w-4 h-4" /> 删除已选
            </button>
          </div>
        </div>

        <div className="flex-1 overflow-y-auto p-4 space-y-4">
          {error && <p className="text-sm text-red-400 bg-red-500/10 rounded-lg px-3 py-2">{error}</p>}
          {message && <p className="text-sm text-green-400 bg-green-500/10 rounded-lg px-3 py-2">{message}</p>}

          {loading ? (
            <div className="flex items-center justify-center py-20">
              <div className="animate-spin rounded-full h-8 w-8 border-2 border-blue-500 border-t-transparent" />
            </div>
          ) : indexRequired ? (
            <div className="text-center py-20">
              <CopyMinus className="w-12 h-12 text-gray-700 mx-auto mb-4" />
              <p className="text-gray-400">文件索引尚未完成</p>
              <p className="text-sm text-gray-500 mt-2">请先点“重建索引”，完成后再查看重复文件。</p>
            </div>
          ) : groups.length === 0 ? (
            <div className="text-center py-20">
              <CopyMinus className="w-12 h-12 text-gray-700 mx-auto mb-4" />
              <p className="text-gray-400">当前没有重复文件</p>
              <p className="text-sm text-gray-500 mt-2">如果保险库里有老文件，先点“重建索引”再查。</p>
            </div>
          ) : (
            <PhotoProvider>
              {groups.map((group) => {
                const keeperPath = keepers[group.contentHash] ?? group.files[0]?.path
                const keeper = group.files.find((file) => file.path === keeperPath) ?? group.files[0]
                const duplicates = group.files.filter((file) => file.path !== keeper.path)
                const selectedCount = duplicates.filter((file) => selections[file.path]).length
                const reclaimable = duplicates.filter((file) => selections[file.path]).reduce((sum, file) => sum + file.size, 0)
                const isExpanded = Boolean(expanded[group.contentHash])

                return (
                  <section key={group.contentHash} className="bg-gray-900 border border-gray-800 rounded-2xl p-4 space-y-4">
                    <div className="flex flex-wrap items-center justify-between gap-3">
                      <button
                        onClick={() => toggleExpanded(group.contentHash)}
                        className="flex min-w-0 flex-1 items-start gap-3 text-left"
                      >
                        <span className="mt-0.5 text-gray-500">
                          {isExpanded ? <ChevronDown className="w-4 h-4" /> : <ChevronRight className="w-4 h-4" />}
                        </span>
                        <div className="min-w-0">
                          <h3 className="text-white font-medium">重复组 {group.files.length} 个文件</h3>
                          <p className="text-xs text-gray-400 truncate">保留：{keeper.path}</p>
                          <p className="text-xs text-gray-500">
                            单文件大小 {formatSize(group.size)}，当前选中 {selectedCount} 个，可释放 {formatSize(reclaimable)}
                          </p>
                        </div>
                      </button>
                      <div className="flex items-center gap-2">
                        {!isExpanded && (
                          <button
                            onClick={() => toggleExpanded(group.contentHash)}
                            className="inline-flex items-center gap-1 px-3 py-1.5 text-xs rounded-lg border border-gray-800 text-gray-300 hover:border-gray-700"
                          >
                            预览
                          </button>
                        )}
                        <button
                          onClick={() => selectGroupDuplicates(group)}
                          className="inline-flex items-center gap-1 px-3 py-1.5 text-xs rounded-lg border border-gray-800 text-gray-300 hover:border-gray-700"
                        >
                          <ShieldCheck className="w-3.5 h-3.5" /> 选择本组重复项
                        </button>
                      </div>
                    </div>

                    {isExpanded && (
                      <>
                        <div className="space-y-2">
                          <div className="text-xs uppercase tracking-[0.2em] text-green-400">保留文件</div>
                          <DuplicateFileCard
                            vaultId={vaultId}
                            file={keeper}
                            isKeeper
                            selected={false}
                            onPreviewVideo={(url, title) => setActiveVideo({ url, title })}
                          />
                        </div>

                        <div className="space-y-3">
                          <div className="text-xs uppercase tracking-[0.2em] text-amber-300">重复文件</div>
                          <div className="grid grid-cols-1 lg:grid-cols-2 gap-3">
                            {duplicates.map((file) => (
                              <DuplicateFileCard
                                key={file.path}
                                vaultId={vaultId}
                                file={file}
                                selected={Boolean(selections[file.path])}
                                onSelectedChange={(checked) => setSelections((prev) => ({ ...prev, [file.path]: checked }))}
                                onMakeKeeper={() => setKeeper(group, file.path)}
                                onPreviewVideo={(url, title) => setActiveVideo({ url, title })}
                              />
                            ))}
                          </div>
                        </div>
                      </>
                    )}
                  </section>
                )
              })}
              {hasMore && (
                <div className="flex justify-center pt-2">
                  <button
                    onClick={() => void loadMoreGroups()}
                    disabled={loadingMore}
                    className="inline-flex items-center gap-2 px-4 py-2 rounded-lg border border-gray-800 text-sm text-gray-200 hover:border-gray-700 disabled:opacity-50"
                  >
                    {loadingMore && <Loader2 className="w-4 h-4 animate-spin" />}
                    {loadingMore ? '加载中...' : '加载更多重复组'}
                  </button>
                </div>
              )}
            </PhotoProvider>
          )}
        </div>
      </div>

      {activeVideo && (
        <VideoPlayer url={activeVideo.url} title={activeVideo.title} onClose={() => setActiveVideo(null)} />
      )}
    </>
  )
}

function DuplicateFileCard({
  vaultId,
  file,
  selected,
  isKeeper,
  onSelectedChange,
  onMakeKeeper,
  onPreviewVideo,
}: {
  vaultId: string
  file: DuplicateFileItem
  selected: boolean
  isKeeper?: boolean
  onSelectedChange?: (checked: boolean) => void
  onMakeKeeper?: () => void
  onPreviewVideo: (url: string, title: string) => void
}) {
  const contentUrl = api.getContentUrl(vaultId, file.path)
  const videoUrl = api.getVideoUrl(vaultId, file.path)
  const { src: previewSrc, status: previewStatus, onPreviewError } = useImagePreviewSrc(
    file.name,
    contentUrl,
    api.getThumbnailUrl(vaultId, file.path),
  )

  return (
    <article className={`rounded-xl border p-3 ${isKeeper ? 'border-green-500/30 bg-green-500/5' : 'border-gray-800 bg-gray-950/60'}`}>
      <div className="flex gap-3">
        <div className="w-28 h-28 rounded-lg overflow-hidden bg-gray-900 border border-gray-800 flex-shrink-0">
          <FilePreview
            vaultId={vaultId}
            file={file}
            previewSrc={previewSrc}
            previewStatus={previewStatus}
            onPreviewError={onPreviewError}
            onPreviewVideo={onPreviewVideo}
          />
        </div>

        <div className="min-w-0 flex-1 space-y-2">
          <div className="flex items-start justify-between gap-3">
            <div className="min-w-0">
              <p className="text-sm font-medium text-white truncate">{file.name}</p>
              <p className="text-xs text-gray-500 break-all">{file.path}</p>
            </div>
            {isKeeper ? (
              <span className="px-2 py-1 rounded-full text-xs bg-green-500/20 text-green-300 flex-shrink-0">保留</span>
            ) : (
              <label className="flex items-center gap-2 text-sm text-gray-300 flex-shrink-0">
                <input type="checkbox" checked={selected} onChange={(e) => onSelectedChange?.(e.target.checked)} />
                删除
              </label>
            )}
          </div>

          <div className="grid grid-cols-2 gap-2 text-xs text-gray-400">
            <div>大小: {formatSize(file.size)}</div>
            <div>修改时间: {formatDate(file.modTime)}</div>
          </div>

          <div className="flex flex-wrap items-center gap-2">
            {isImage(file.name) && previewSrc && (
              <PhotoView src={previewSrc}>
                <button className="px-2.5 py-1.5 rounded-lg border border-gray-800 text-xs text-gray-200 hover:border-gray-700">预览图片</button>
              </PhotoView>
            )}
            {isVideo(file.name) && (
              <button
                onClick={() => onPreviewVideo(videoUrl, file.name)}
                className="px-2.5 py-1.5 rounded-lg border border-gray-800 text-xs text-gray-200 hover:border-gray-700"
              >
                预览视频
              </button>
            )}
            {!isKeeper && (
              <button
                onClick={onMakeKeeper}
                className="px-2.5 py-1.5 rounded-lg border border-gray-800 text-xs text-gray-200 hover:border-gray-700"
              >
                设为保留文件
              </button>
            )}
          </div>
        </div>
      </div>
    </article>
  )
}

function FilePreview({
  vaultId,
  file,
  previewSrc,
  previewStatus,
  onPreviewError,
  onPreviewVideo,
}: {
  vaultId: string
  file: DuplicateFileItem
  previewSrc: string
  previewStatus: 'loading' | 'ready' | 'unavailable'
  onPreviewError: () => void
  onPreviewVideo: (url: string, title: string) => void
}) {
  if (isImage(file.name)) {
    if (previewStatus !== 'ready' || !previewSrc) {
      return (
        <div className="w-full h-full flex items-center justify-center text-gray-500">
          {previewStatus === 'loading' ? <Loader2 className="w-5 h-5 animate-spin" /> : <ImageIcon className="w-6 h-6" />}
        </div>
      )
    }
    return <img src={previewSrc} alt={file.name} className="w-full h-full object-cover" onError={onPreviewError} />
  }

  if (isVideo(file.name)) {
    if (file.hasThumb) {
      return (
        <button className="w-full h-full" onClick={() => onPreviewVideo(api.getVideoUrl(vaultId, file.path), file.name)}>
          <img src={api.getThumbnailUrl(vaultId, file.path)} alt={file.name} className="w-full h-full object-cover" />
        </button>
      )
    }
    return <div className="w-full h-full flex items-center justify-center text-purple-400"><Film className="w-7 h-7" /></div>
  }

  return <div className="w-full h-full flex items-center justify-center text-gray-500"><FileIcon className="w-7 h-7" /></div>
}
