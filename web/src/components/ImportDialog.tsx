import { useState, useEffect } from 'react'
import { api, type DirEntry, formatSize, joinPath } from '../lib/api'
import { X, Folder, File, ArrowLeft, FolderInput } from 'lucide-react'

import Breadcrumbs from './Breadcrumbs'
interface ImportDialogProps {
  vaultId: string
  onClose: () => void
  onStarted: () => void
}

export default function ImportDialog({ vaultId, onClose, onStarted }: ImportDialogProps) {
  const [currentPath, setCurrentPath] = useState('/data')
  const [items, setItems] = useState<DirEntry[]>([])
  const [loading, setLoading] = useState(true)
  const [destPath, setDestPath] = useState('/')
  const [deleteSource, setDeleteSource] = useState(false)
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState('')

  useEffect(() => {
    loadDir(currentPath)
  }, [currentPath])

  async function loadDir(path: string) {
    setLoading(true)
    setError('')
    try {
      const data = await api.browseDir(path)
      setItems(data.items || [])
      setCurrentPath(data.path)
    } catch (err) {
      setError(err instanceof Error ? err.message : '无法读取目录')
      setItems([])
    } finally {
      setLoading(false)
    }
  }

  function navigateTo(path: string) {
    setCurrentPath(path)
  }

  function goUp() {
    const parts = currentPath.split('/').filter(Boolean)
    if (parts.length <= 1) return
    parts.pop()
    navigateTo('/' + parts.join('/'))
  }

  function enterDir(name: string) {
    navigateTo(joinPath(currentPath, name))
  }

  async function handleSubmit() {
    setError('')
    setSubmitting(true)
    try {
      await api.createImportTask(vaultId, currentPath, destPath, deleteSource)
      onStarted()
    } catch (err) {
      setError(err instanceof Error ? err.message : '启动导入失败')
    } finally {
      setSubmitting(false)
    }
  }

  const dirCount = items.filter(i => i.isDir).length
  const fileCount = items.filter(i => !i.isDir).length
  return (
    <div className="fixed inset-0 bg-black/60 flex items-center justify-center z-50 p-2 sm:p-4" onClick={onClose}>
      <div
        className="bg-gray-900 border border-gray-800 rounded-2xl w-full max-w-lg max-h-[calc(100vh-2rem)] sm:max-h-[85vh] flex flex-col"
        onClick={e => e.stopPropagation()}
      >
        {/* Header */}
        <div className="flex items-center justify-between p-4 border-b border-gray-800">
          <div className="flex items-center gap-2">
            <FolderInput className="w-5 h-5 text-amber-500" />
            <h2 className="text-lg font-semibold text-white">导入目录</h2>
          </div>
          <button onClick={onClose} className="p-1 text-gray-400 hover:text-white">
            <X className="w-5 h-5" />
          </button>
        </div>

        {/* Breadcrumb */}
        <div className="px-4 py-2 border-b border-gray-800/50 flex-shrink-0">
          <Breadcrumbs path={currentPath} onNavigate={navigateTo} rootPath="/data" />
        </div>

        {/* Directory listing */}
        <div className="flex-1 overflow-y-auto min-h-0" style={{ maxHeight: '300px' }}>
          {loading ? (
            <div className="flex items-center justify-center py-12">
              <div className="animate-spin rounded-full h-6 w-6 border-2 border-blue-500 border-t-transparent" />
            </div>
          ) : (
            <div className="p-2">
              {currentPath !== '/data' && (
                <button
                  onClick={goUp}
                  className="w-full flex items-center gap-3 px-3 py-2 rounded-lg hover:bg-gray-800 transition-colors text-left"
                >
                  <ArrowLeft className="w-4 h-4 text-gray-400" />
                  <span className="text-sm text-gray-400">返回上级</span>
                </button>
              )}
              {items.length === 0 && (
                <p className="text-center text-gray-500 text-sm py-8">空目录</p>
              )}
              {items.map(item => (
                <div
                  key={item.name}
                  className={`flex items-center gap-3 px-3 py-2 rounded-lg transition-colors ${item.isDir ? 'hover:bg-gray-800 cursor-pointer' : ''}`}
                  onClick={() => item.isDir && enterDir(item.name)}
                >
                  {item.isDir
                    ? <Folder className="w-4 h-4 text-amber-500 flex-shrink-0" />
                    : <File className="w-4 h-4 text-gray-500 flex-shrink-0" />}
                  <span className="text-sm text-gray-300 truncate flex-1">{item.name}</span>
                  {!item.isDir && <span className="text-xs text-gray-500">{formatSize(item.size)}</span>}
                </div>
              ))}
            </div>
          )}
        </div>

        {/* Stats */}
        <div className="px-4 py-2 border-t border-gray-800/50 text-xs text-gray-500">
          当前目录: {dirCount} 个文件夹, {fileCount} 个文件
        </div>

        {/* Options */}
        <div className="p-4 border-t border-gray-800 space-y-3">
          <div>
            <label className="block text-sm text-gray-400 mb-1">保险库目标路径</label>
            <input
              type="text"
              value={destPath}
              onChange={e => setDestPath(e.target.value)}
              className="w-full bg-gray-800 border border-gray-700 rounded-xl px-4 py-2.5 text-white text-sm placeholder-gray-500 focus:outline-none focus:ring-2 focus:ring-blue-500 focus:border-transparent"
              placeholder="/"
            />
          </div>
          <label className="flex items-center gap-2 cursor-pointer">
            <input
              type="checkbox"
              checked={deleteSource}
              onChange={e => setDeleteSource(e.target.checked)}
              className="w-4 h-4 rounded border-gray-600 bg-gray-800 text-blue-500 focus:ring-blue-500 focus:ring-offset-0"
            />
            <span className="text-sm text-gray-300">导入后删除原文件</span>
          </label>

          {error && <p className="text-red-400 text-sm">{error}</p>}

          <div className="flex gap-3">
            <button
              onClick={onClose}
              className="flex-1 bg-gray-800 hover:bg-gray-700 text-gray-300 py-2.5 rounded-xl font-medium transition-colors text-sm"
            >
              取消
            </button>
            <button
              onClick={handleSubmit}
              disabled={submitting || loading}
              className="flex-1 bg-blue-600 hover:bg-blue-700 disabled:opacity-50 text-white py-2.5 rounded-xl font-medium transition-colors text-sm"
            >
              {submitting ? '启动中...' : '开始导入'}
            </button>
          </div>
        </div>
      </div>
    </div>
  )
}
