import { useState, useEffect, useCallback, useRef } from 'react'
import { useParams, useNavigate } from 'react-router-dom'
import { PhotoProvider } from 'react-photo-view'
import { api, ApiError, type FileItem, joinPath } from '../lib/api'
import { Folder, ArrowLeft, Grid3x3, List, Upload, FolderPlus, Lock, ListTodo, FolderInput, ArrowUpDown, CopyMinus } from 'lucide-react'
import VideoPlayer from '../components/VideoPlayer'
import UploadDialog from '../components/UploadDialog'
import CreateFolderDialog from '../components/CreateFolderDialog'
import TaskPanel from '../components/TaskPanel'
import ImportDialog from '../components/ImportDialog'
import DuplicatePanel from '../components/DuplicatePanel'
import FileGridItem from '../components/FileGridItem'
import FileListItem from '../components/FileListItem'
import Breadcrumbs from '../components/Breadcrumbs'

type SortField = 'name' | 'modTime' | 'size'
type SortDirection = 'asc' | 'desc'

const SORT_STORAGE_KEY = 'fileBrowserSort'

const PAGE_SIZE = 100

export default function FileBrowser() {
  const { id: vaultId } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const [currentPath, setCurrentPath] = useState('/')
  const [files, setFiles] = useState<FileItem[]>([])
  const [loading, setLoading] = useState(true)
  const [loadingMore, setLoadingMore] = useState(false)
  const [hasMore, setHasMore] = useState(false)
  const [nextOffset, setNextOffset] = useState(0)
  const [viewMode, setViewMode] = useState<'grid' | 'list'>('grid')
  const [activeVideo, setActiveVideo] = useState<{ url: string; title: string } | null>(null)
  const [showUpload, setShowUpload] = useState(false)
  const [showMkdir, setShowMkdir] = useState(false)
  const [showTasks, setShowTasks] = useState(false)
  const [showImport, setShowImport] = useState(false)
  const [showDuplicates, setShowDuplicates] = useState(false)
  const [indexRequired, setIndexRequired] = useState(false)
  const [error, setError] = useState('')
  const [sortField, setSortField] = useState<SortField>(() => {
    const raw = localStorage.getItem(SORT_STORAGE_KEY)
    if (!raw) return 'name'
    try {
      const parsed = JSON.parse(raw) as { field?: SortField }
      return parsed.field === 'modTime' || parsed.field === 'size' || parsed.field === 'name' ? parsed.field : 'name'
    } catch {
      return 'name'
    }
  })
  const [sortDirection, setSortDirection] = useState<SortDirection>(() => {
    const raw = localStorage.getItem(SORT_STORAGE_KEY)
    if (!raw) return 'asc'
    try {
      const parsed = JSON.parse(raw) as { direction?: SortDirection }
      return parsed.direction === 'desc' ? 'desc' : 'asc'
    } catch {
      return 'asc'
    }
  })
  const loadMoreRef = useRef<HTMLDivElement | null>(null)
  const listRequestIdRef = useRef(0)
  const listAbortRef = useRef<AbortController | null>(null)
  const loadingMoreRef = useRef(false)
  const cancelListRequest = useCallback(() => {
    listRequestIdRef.current += 1
    listAbortRef.current?.abort()
  }, [])

  const loadFiles = useCallback(async (offset = 0, append = false) => {
    if (!vaultId) return
    if (append && loadingMoreRef.current) return

    const requestId = ++listRequestIdRef.current
    listAbortRef.current?.abort()
    const controller = new AbortController()
    listAbortRef.current = controller
    if (append) {
      loadingMoreRef.current = true
      setLoadingMore(true)
    } else {
      loadingMoreRef.current = false
      setLoadingMore(false)
      setLoading(true)
      setError('')
    }
    try {
      const data = await api.listFiles(vaultId, currentPath, {
        offset,
        limit: PAGE_SIZE,
        sortField,
        sortDirection,
        signal: controller.signal,
      })
      if (requestId !== listRequestIdRef.current) return
      const nextFiles = data.files || []
      setIndexRequired(Boolean(data.indexRequired))
      setFiles((prev) => {
        if (!append) return nextFiles
        const seen = new Set(prev.map((file) => file.name))
        return [...prev, ...nextFiles.filter((file) => {
          if (seen.has(file.name)) return false
          seen.add(file.name)
          return true
        })]
      })
      setHasMore(Boolean(data.hasMore))
      setNextOffset(data.nextOffset ?? (offset + nextFiles.length))
    } catch (err) {
      if (controller.signal.aborted || requestId !== listRequestIdRef.current) return
      if (err instanceof ApiError && (err.status === 401 || err.status === 403)) {
        navigate('/')
        return
      }
      setError(err instanceof Error ? err.message : '加载失败')
    } finally {
      if (requestId === listRequestIdRef.current) {
        if (append) {
          loadingMoreRef.current = false
          setLoadingMore(false)
        } else {
          setLoading(false)
        }
      }
    }
  }, [vaultId, currentPath, navigate, sortField, sortDirection])

  useEffect(() => {
    cancelListRequest()
    loadingMoreRef.current = false
    setLoadingMore(false)
    setFiles([])
    setHasMore(false)
    setNextOffset(0)
    void loadFiles(0, false)
    return () => {
      cancelListRequest()
    }
  }, [cancelListRequest, loadFiles])
  useEffect(() => {
    localStorage.setItem(SORT_STORAGE_KEY, JSON.stringify({ field: sortField, direction: sortDirection }))
  }, [sortField, sortDirection])

  useEffect(() => {
    if (!hasMore || loading || loadingMore) return
    const target = loadMoreRef.current
    if (!target) return

    const observer = new IntersectionObserver((entries) => {
      const entry = entries[0]
      if (entry?.isIntersecting) {
        void loadFiles(nextOffset, true)
      }
    }, { rootMargin: '200px 0px' })

    observer.observe(target)
    return () => observer.disconnect()
  }, [hasMore, loading, loadingMore, nextOffset, loadFiles])

  function navigateTo(path: string) {
    setFiles([])
    setCurrentPath(path)
  }

  function goUp() {
    if (currentPath === '/') return
    const parts = currentPath.split('/').filter(Boolean)
    parts.pop()
    navigateTo('/' + parts.join('/'))
  }

  function openDir(name: string) {
    const newPath = joinPath(currentPath, name)
    navigateTo(newPath)
  }

  function getFilePath(name: string) {
    return joinPath(currentPath, name)
  }

  async function handleDelete(file: FileItem) {
    if (!vaultId) return
    if (!confirm(`确定要删除 "${file.name}" 吗？`)) return
    setError('')
    try {
      await api.deleteFile(vaultId, getFilePath(file.name))
      void loadFiles()
    } catch (err) {
      setError(err instanceof Error ? err.message : '删除失败')
    }
  }

  function handleDownload(file: FileItem) {
    if (!vaultId) return
    const link = document.createElement('a')
    link.href = api.getDownloadUrl(vaultId, getFilePath(file.name))
    link.rel = 'noopener'
    document.body.appendChild(link)
    link.click()
    link.remove()
  }

  async function handleRebuildIndex() {
    if (!vaultId) return
    setError('')
    try {
      await api.rebuildFileIndex(vaultId)
      setShowTasks(true)
    } catch (err) {
      setError(err instanceof Error ? err.message : '重建索引失败')
    }
  }

  function handleToggleDirection() {
    setSortDirection((prev) => prev === 'asc' ? 'desc' : 'asc')
  }

  return (
    <div className="min-h-screen flex flex-col">
      {/* Header */}
      <header className="border-b border-gray-800 bg-gray-950/80 backdrop-blur-sm sticky top-0 z-10">
        <div className="max-w-7xl mx-auto px-4 h-14 flex items-center gap-3">
          {/* Back */}
          <button
            onClick={currentPath === '/' ? () => navigate('/') : goUp}
            className="p-2 text-gray-400 hover:text-white transition-colors"
          >
            {currentPath === '/' ? <Lock className="w-5 h-5" /> : <ArrowLeft className="w-5 h-5" />}
          </button>

          {/* Breadcrumb */}
          <Breadcrumbs path={currentPath} onNavigate={navigateTo} />

          {/* Actions */}
          <div className="flex items-center gap-1 flex-shrink-0">
            <button onClick={() => setShowMkdir(true)} className="p-2 text-gray-400 hover:text-white transition-colors" title="新建文件夹">
              <FolderPlus className="w-5 h-5" />
            </button>
            <button onClick={() => setShowUpload(true)} className="p-2 text-gray-400 hover:text-white transition-colors" title="上传文件">
              <Upload className="w-5 h-5" />
            </button>
            <button onClick={() => setShowImport(true)} className="p-2 text-gray-400 hover:text-white transition-colors" title="导入目录">
              <FolderInput className="w-5 h-5" />
            </button>
            <button onClick={() => setShowDuplicates(true)} className="p-2 text-gray-400 hover:text-white transition-colors" title="重复文件">
              <CopyMinus className="w-5 h-5" />
            </button>
            <button onClick={() => setShowTasks(true)} className="p-2 text-gray-400 hover:text-white transition-colors" title="任务列表">
              <ListTodo className="w-5 h-5" />
            </button>
            <div className="w-px h-5 bg-gray-800 mx-1" />
            <button
              onClick={() => setViewMode(viewMode === 'grid' ? 'list' : 'grid')}
              className="p-2 text-gray-400 hover:text-white transition-colors"
              title={viewMode === 'grid' ? '列表视图' : '网格视图'}
            >
              {viewMode === 'grid' ? <List className="w-5 h-5" /> : <Grid3x3 className="w-5 h-5" />}
            </button>
          </div>
        </div>
      </header>

      {/* Content */}
      <main className="flex-1 max-w-7xl w-full mx-auto p-4">
        <div className="mb-4 flex items-center justify-between gap-3">
          <div className="text-xs text-gray-500">
            {currentPath === '/' ? '根目录' : currentPath}
          </div>
          <div className="flex items-center gap-2">
            <label className="text-xs text-gray-500" htmlFor="file-sort">排序</label>
            <select
              id="file-sort"
              value={sortField}
              onChange={(e) => setSortField(e.target.value as SortField)}
              className="h-9 rounded-lg border border-gray-800 bg-gray-950 px-3 text-sm text-gray-200 outline-none transition-colors hover:border-gray-700 focus:border-gray-600"
            >
              <option value="name">文件名称</option>
              <option value="modTime">修改时间</option>
              <option value="size">文件大小</option>
            </select>
            <button
              onClick={handleToggleDirection}
              className="inline-flex h-9 items-center gap-2 rounded-lg border border-gray-800 bg-gray-950 px-3 text-sm text-gray-200 transition-colors hover:border-gray-700"
              title={sortDirection === 'asc' ? '当前升序' : '当前降序'}
            >
              <ArrowUpDown className="h-4 w-4" />
              {sortDirection === 'asc' ? '升序' : '降序'}
            </button>
          </div>
        </div>

        {loading ? (
          <div className="flex items-center justify-center py-20">
            <div className="animate-spin rounded-full h-8 w-8 border-2 border-blue-500 border-t-transparent" />
          </div>
        ) : error ? (
          <div className="text-center py-20">
            <p className="text-red-400">{error}</p>
            <button onClick={() => void loadFiles(0, false)} className="mt-4 text-blue-500 hover:text-blue-400">重试</button>
          </div>
        ) : indexRequired ? (
          <div className="text-center py-20">
            <Folder className="w-12 h-12 text-gray-700 mx-auto mb-4" />
            <p className="text-gray-400">此目录尚未建立索引</p>
            <p className="text-sm text-gray-500 mt-2">重建将同时生成媒体元数据和缩略图。</p>
            <button onClick={handleRebuildIndex} className="mt-4 text-blue-500 hover:text-blue-400 text-sm">重建索引</button>
          </div>
        ) : files.length === 0 ? (
          <div className="text-center py-20">
            <Folder className="w-12 h-12 text-gray-700 mx-auto mb-4" />
            <p className="text-gray-500">文件夹为空</p>
            <div className="flex gap-3 justify-center mt-4">
              <button onClick={() => setShowUpload(true)} className="text-blue-500 hover:text-blue-400 text-sm">上传文件</button>
              <button onClick={() => setShowMkdir(true)} className="text-blue-500 hover:text-blue-400 text-sm">新建文件夹</button>
            </div>
          </div>
        ) : (
          <PhotoProvider>
            {viewMode === 'grid' ? (
              <div className="grid grid-cols-2 sm:grid-cols-3 md:grid-cols-4 lg:grid-cols-5 xl:grid-cols-6 gap-3">
                {files.map((file) => (
                  <FileGridItem
                    key={file.name}
                    file={file}
                    vaultId={vaultId!}
                    currentPath={currentPath}
                    onOpenDir={openDir}
                    onPlayVideo={(url, title) => setActiveVideo({ url, title })}
                    onDelete={handleDelete}
                    onDownload={handleDownload}
                  />
                ))}
              </div>
            ) : (
              <div className="space-y-1">
                {files.map((file) => (
                  <FileListItem
                    key={file.name}
                    file={file}
                    vaultId={vaultId!}
                    currentPath={currentPath}
                    onOpenDir={openDir}
                    onPlayVideo={(url, title) => setActiveVideo({ url, title })}
                    onDelete={handleDelete}
                    onDownload={handleDownload}
                  />
                ))}
              </div>
            )}
            {(hasMore || loadingMore) && (
              <div ref={loadMoreRef} className="flex items-center justify-center py-6 text-sm text-gray-500">
                {loadingMore ? '加载更多文件中...' : '继续下滑加载更多'}
              </div>
            )}
          </PhotoProvider>
        )}
      </main>

      {/* Modals */}
      {activeVideo && (
        <VideoPlayer url={activeVideo.url} title={activeVideo.title} onClose={() => setActiveVideo(null)} />
      )}
      {showUpload && vaultId && (
        <UploadDialog vaultId={vaultId} currentPath={currentPath} onClose={() => setShowUpload(false)} onUploaded={loadFiles} onTaskCreated={() => setShowTasks(true)} />
      )}
      {showMkdir && vaultId && (
        <CreateFolderDialog vaultId={vaultId} currentPath={currentPath} onClose={() => setShowMkdir(false)} onCreated={loadFiles} />
      )}
      {showImport && vaultId && (
        <ImportDialog vaultId={vaultId} onClose={() => setShowImport(false)} onStarted={() => { setShowImport(false); setShowTasks(true) }} />
      )}
      {vaultId && showTasks && (
        <TaskPanel vaultId={vaultId} open onClose={() => setShowTasks(false)} onRefresh={loadFiles} />
      )}
      {vaultId && showDuplicates && (
        <DuplicatePanel
          vaultId={vaultId}
          open
          onClose={() => setShowDuplicates(false)}
          onRefresh={loadFiles}
          onOpenTasks={() => setShowTasks(true)}
        />
      )}
    </div>
  )
}
