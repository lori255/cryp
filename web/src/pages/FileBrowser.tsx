import { useState, useEffect, useCallback } from 'react'
import { useParams, useNavigate } from 'react-router-dom'
import { PhotoProvider } from 'react-photo-view'
import { api, type FileItem, joinPath } from '../lib/api'
import { Folder, ArrowLeft, Grid3x3, List, Upload, FolderPlus, Lock, ListTodo, FolderInput } from 'lucide-react'
import VideoPlayer from '../components/VideoPlayer'
import UploadDialog from '../components/UploadDialog'
import CreateFolderDialog from '../components/CreateFolderDialog'
import TaskPanel from '../components/TaskPanel'
import ImportDialog from '../components/ImportDialog'
import FileGridItem from '../components/FileGridItem'
import FileListItem from '../components/FileListItem'
import Breadcrumbs from '../components/Breadcrumbs'

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
      loadFiles()
    } catch (err) {
      setError(err instanceof Error ? err.message : '删除失败')
    }
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