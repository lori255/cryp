import { useState, useEffect, useCallback } from 'react'
import { useParams, useNavigate } from 'react-router-dom'
import { PhotoProvider, PhotoView } from 'react-photo-view'
import { api, type FileItem, isImage, isVideo, formatSize, formatDate } from '../lib/api'
import { Folder, File, Film, Image, ArrowLeft, Grid3x3, List, Upload, FolderPlus, Trash2, Home, ChevronRight, Lock, Music, ListTodo, FolderInput } from 'lucide-react'
import VideoPlayer from '../components/VideoPlayer'
import UploadDialog from '../components/UploadDialog'
import CreateFolderDialog from '../components/CreateFolderDialog'
import TaskPanel from '../components/TaskPanel'
import ImportDialog from '../components/ImportDialog'

export default function FileBrowser() {
  const { id: vaultId } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const [currentPath, setCurrentPath] = useState('/')
  const [files, setFiles] = useState<FileItem[]>([])
  const [loading, setLoading] = useState(true)
  const [viewMode, setViewMode] = useState<'grid' | 'list'>('grid')
  const [activeVideo, setActiveVideo] = useState<{ url: string; title: string } | null>(null)
  const [showUpload, setShowUpload] = useState(false)
  const [showMkdir, setShowMkdir] = useState(false)
  const [showTasks, setShowTasks] = useState(false)
  const [showImport, setShowImport] = useState(false)
  const [error, setError] = useState('')

  const loadFiles = useCallback(async () => {
    if (!vaultId) return
    setLoading(true)
    setError('')
    try {
      const data = await api.listFiles(vaultId, currentPath)
      // Sort: dirs first, then alphabetical
      const sorted = (data.files || []).sort((a, b) => {
        if (a.isDir !== b.isDir) return a.isDir ? -1 : 1
        return a.name.localeCompare(b.name)
      })
      setFiles(sorted)
    } catch (err) {
      if (err instanceof Error && err.message.includes('session')) {
        navigate('/')
        return
      }
      setError(err instanceof Error ? err.message : '加载失败')
    } finally {
      setLoading(false)
    }
  }, [vaultId, currentPath, navigate])

  useEffect(() => { loadFiles() }, [loadFiles])

  function navigateTo(path: string) {
    setCurrentPath(path)
  }

  function goUp() {
    if (currentPath === '/') return
    const parts = currentPath.split('/').filter(Boolean)
    parts.pop()
    navigateTo('/' + parts.join('/'))
  }

  function openDir(name: string) {
    const newPath = currentPath === '/' ? `/${name}` : `${currentPath}/${name}`
    navigateTo(newPath)
  }

  function getFilePath(name: string) {
    return currentPath === '/' ? `/${name}` : `${currentPath}/${name}`
  }

  async function handleDelete(file: FileItem) {
    if (!vaultId) return
    if (!confirm(`确定要删除 "${file.name}" 吗？`)) return
    try {
      await api.deleteFile(vaultId, getFilePath(file.name))
      loadFiles()
    } catch { /* empty */ }
  }

  // Breadcrumb
  const pathParts = currentPath.split('/').filter(Boolean)

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
          <nav className="flex items-center gap-1 text-sm flex-1 min-w-0 overflow-x-auto">
            <button onClick={() => navigateTo('/')} className="text-gray-400 hover:text-white transition-colors flex-shrink-0">
              <Home className="w-4 h-4" />
            </button>
            {pathParts.map((part, i) => (
              <span key={i} className="flex items-center gap-1 flex-shrink-0">
                <ChevronRight className="w-3 h-3 text-gray-600" />
                <button
                  onClick={() => navigateTo('/' + pathParts.slice(0, i + 1).join('/'))}
                  className={`hover:text-white transition-colors ${i === pathParts.length - 1 ? 'text-white font-medium' : 'text-gray-400'}`}
                >
                  {part}
                </button>
              </span>
            ))}
          </nav>

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
        {loading ? (
          <div className="flex items-center justify-center py-20">
            <div className="animate-spin rounded-full h-8 w-8 border-2 border-blue-500 border-t-transparent" />
          </div>
        ) : error ? (
          <div className="text-center py-20">
            <p className="text-red-400">{error}</p>
            <button onClick={loadFiles} className="mt-4 text-blue-500 hover:text-blue-400">重试</button>
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
                  />
                ))}
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
      {vaultId && (
        <TaskPanel vaultId={vaultId} open={showTasks} onClose={() => setShowTasks(false)} onRefresh={loadFiles} />
      )}
    </div>
  )
}

// --- Video Thumbnail ---
function VideoThumbnail({ src, alt }: { src: string; alt: string }) {
  const [thumb, setThumb] = useState<string | null>(null)
  const [failed, setFailed] = useState(false)

  useEffect(() => {
    if (failed) return
    const video = document.createElement('video')
    // Don't set crossOrigin for same-origin requests — iOS Safari blocks canvas
    // with crossOrigin='anonymous' even when CORS headers are present
    video.preload = 'auto'
    video.muted = true
    // Required for iOS Safari: without playsinline, iOS won't load video in background
    video.setAttribute('playsinline', '')
    video.setAttribute('webkit-playsinline', '')

    let cancelled = false

    const cleanup = () => {
      cancelled = true
      video.removeAttribute('src')
      video.load()
    }

    video.addEventListener('loadeddata', () => {
      if (cancelled) return
      // On iOS, seeking to 0 is more reliable than seeking to 1
      video.currentTime = Math.min(1, video.duration || 1)
    })

    video.addEventListener('seeked', () => {
      if (cancelled) return
      try {
        const canvas = document.createElement('canvas')
        canvas.width = video.videoWidth || 320
        canvas.height = video.videoHeight || 180
        const ctx = canvas.getContext('2d')
        if (ctx) {
          ctx.drawImage(video, 0, 0, canvas.width, canvas.height)
          setThumb(canvas.toDataURL('image/jpeg', 0.7))
        }
      } catch {
        setFailed(true)
      }
      cleanup()
    })

    video.addEventListener('error', () => {
      setFailed(true)
      cleanup()
    })

    // iOS needs a brief delay before setting src for programmatic video loading
    video.src = src
    // Explicitly call load() — iOS Safari won't auto-load without it
    video.load()
    return cleanup
  }, [src, failed])

  if (thumb) {
    return <img src={thumb} alt={alt} className="w-full h-full object-cover" />
  }

  return <Film className="w-10 h-10 text-purple-500" />
}

// --- Grid Item ---
function FileGridItem({ file, vaultId, currentPath, onOpenDir, onPlayVideo, onDelete }: {
  file: FileItem
  vaultId: string
  currentPath: string
  onOpenDir: (name: string) => void
  onPlayVideo: (url: string, title: string) => void
  onDelete: (file: FileItem) => void
}) {
  const filePath = currentPath === '/' ? `/${file.name}` : `${currentPath}/${file.name}`

  if (file.isDir) {
    return (
      <button onClick={() => onOpenDir(file.name)}
        className="bg-gray-900 border border-gray-800 hover:border-gray-700 rounded-xl p-4 flex flex-col items-center gap-2 transition-all group text-center">
        <Folder className="w-10 h-10 text-amber-500" />
        <span className="text-sm text-gray-300 truncate w-full">{file.name}</span>
        <button onClick={(e) => { e.stopPropagation(); onDelete(file) }}
          className="absolute top-2 right-2 p-1 text-gray-600 hover:text-red-400 sm:opacity-0 sm:group-hover:opacity-100 transition-all">
          <Trash2 className="w-3.5 h-3.5" />
        </button>
      </button>
    )
  }

  if (isImage(file.name)) {
    const contentUrl = api.getContentUrl(vaultId, filePath)
    return (
      <div className="relative group">
        <PhotoView src={contentUrl}>
          <div className="bg-gray-900 border border-gray-800 hover:border-gray-700 rounded-xl overflow-hidden cursor-pointer transition-all">
            <div className="aspect-square bg-gray-800">
              <img src={contentUrl} alt={file.name} className="w-full h-full object-cover" loading="lazy" />
            </div>
            <div className="p-2">
              <p className="text-xs text-gray-400 truncate">{file.name}</p>
            </div>
          </div>
        </PhotoView>
        <button onClick={() => onDelete(file)}
          className="absolute top-2 right-2 p-1 bg-black/50 rounded text-gray-400 hover:text-red-400 sm:opacity-0 sm:group-hover:opacity-100 transition-all z-10">
          <Trash2 className="w-3.5 h-3.5" />
        </button>
      </div>
    )
  }

  if (isVideo(file.name)) {
    const contentUrl = api.getContentUrl(vaultId, filePath)
    return (
      <div className="relative group">
        <button onClick={() => onPlayVideo(contentUrl, file.name)}
          className="w-full bg-gray-900 border border-gray-800 hover:border-gray-700 rounded-xl overflow-hidden transition-all text-center">
          <div className="aspect-video bg-gray-800 flex items-center justify-center">
            <VideoThumbnail src={contentUrl} alt={file.name} />
          </div>
          <div className="p-2">
            <p className="text-xs text-gray-400 truncate">{file.name}</p>
            <p className="text-xs text-gray-500">{formatSize(file.size)}</p>
          </div>
        </button>
        <button onClick={() => onDelete(file)}
          className="absolute top-2 right-2 p-1 bg-black/50 rounded text-gray-400 hover:text-red-400 sm:opacity-0 sm:group-hover:opacity-100 transition-all z-10">
          <Trash2 className="w-3.5 h-3.5" />
        </button>
      </div>
    )
  }

  return (
    <div className="relative group">
      <div className="bg-gray-900 border border-gray-800 rounded-xl p-4 flex flex-col items-center gap-2 text-center">
        {file.name.match(/\.(mp3|wav|ogg|flac)$/i) ? <Music className="w-10 h-10 text-green-500" /> :
         file.name.match(/\.(jpg|jpeg|png|gif)$/i) ? <Image className="w-10 h-10 text-blue-500" /> :
         <File className="w-10 h-10 text-gray-500" />}
        <span className="text-sm text-gray-300 truncate w-full">{file.name}</span>
        <span className="text-xs text-gray-500">{formatSize(file.size)}</span>
      </div>
      <button onClick={() => onDelete(file)}
        className="absolute top-2 right-2 p-1 text-gray-600 hover:text-red-400 sm:opacity-0 sm:group-hover:opacity-100 transition-all">
        <Trash2 className="w-3.5 h-3.5" />
      </button>
    </div>
  )
}

// --- List Item ---
function FileListItem({ file, vaultId, currentPath, onOpenDir, onPlayVideo, onDelete }: {
  file: FileItem
  vaultId: string
  currentPath: string
  onOpenDir: (name: string) => void
  onPlayVideo: (url: string, title: string) => void
  onDelete: (file: FileItem) => void
}) {
  const filePath = currentPath === '/' ? `/${file.name}` : `${currentPath}/${file.name}`

  function handleClick() {
    if (file.isDir) {
      onOpenDir(file.name)
    } else if (isVideo(file.name)) {
      onPlayVideo(api.getContentUrl(vaultId, filePath), file.name)
    }
  }

  const icon = file.isDir ? <Folder className="w-5 h-5 text-amber-500" /> :
    isImage(file.name) ? <Image className="w-5 h-5 text-blue-500" /> :
    isVideo(file.name) ? <Film className="w-5 h-5 text-purple-500" /> :
    <File className="w-5 h-5 text-gray-500" />

  const row = (
    <div
      className="flex items-center gap-3 px-3 py-2.5 rounded-lg hover:bg-gray-900 transition-colors group cursor-pointer"
      onClick={handleClick}
    >
      {icon}
      <span className="flex-1 text-sm text-gray-200 truncate">{file.name}</span>
      {!file.isDir && <span className="text-xs text-gray-500 flex-shrink-0">{formatSize(file.size)}</span>}
      {file.modTime > 0 && <span className="text-xs text-gray-600 flex-shrink-0 hidden sm:block">{formatDate(file.modTime)}</span>}
      <button
        onClick={(e) => { e.stopPropagation(); onDelete(file) }}
        className="p-1 text-gray-600 hover:text-red-400 sm:opacity-0 sm:group-hover:opacity-100 transition-all flex-shrink-0"
      >
        <Trash2 className="w-4 h-4" />
      </button>
    </div>
  )

  if (isImage(file.name)) {
    const contentUrl = api.getContentUrl(vaultId, filePath)
    return <PhotoView src={contentUrl}>{row}</PhotoView>
  }

  return row
}
