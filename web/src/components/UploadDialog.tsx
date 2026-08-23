import { useState, useRef, useEffect } from 'react'
import { api, formatSize } from '../lib/api'
import { Upload, X, FileUp, CheckCircle2, AlertCircle } from 'lucide-react'

interface UploadDialogProps {
  vaultId: string
  currentPath: string
  onClose: () => void
  onUploaded: () => void
  onTaskCreated?: () => void
}

interface UploadItem {
  file: File
  progress: number
  status: 'pending' | 'uploading' | 'done' | 'error'
  error?: string
}

export default function UploadDialog({ vaultId, currentPath, onClose, onUploaded, onTaskCreated }: UploadDialogProps) {
  const [items, setItems] = useState<UploadItem[]>([])
  const [uploading, setUploading] = useState(false)
  const [dragOver, setDragOver] = useState(false)
  const inputRef = useRef<HTMLInputElement>(null)
  const uploadAbortRef = useRef<AbortController | null>(null)

  useEffect(() => () => {
    uploadAbortRef.current?.abort()
  }, [])

  function handleClose() {
    uploadAbortRef.current?.abort()
    onClose()
  }

  function addFiles(fileList: FileList | null) {
    if (!fileList) return
    const newItems: UploadItem[] = Array.from(fileList).map((file) => ({
      file,
      progress: 0,
      status: 'pending' as const,
    }))
    setItems((prev) => [...prev, ...newItems])
  }

  function handleDrop(e: React.DragEvent) {
    e.preventDefault()
    setDragOver(false)
    addFiles(e.dataTransfer.files)
  }

  async function uploadAll() {
    setUploading(true)
    const controller = new AbortController()
    uploadAbortRef.current = controller

    const pendingItems = items.filter(i => i.status === 'pending')
    const totalBytes = pendingItems.reduce((sum, i) => sum + i.file.size, 0)

    let taskId: string | undefined
    try {
      const resp = await api.createUploadTask(vaultId, pendingItems.length, totalBytes)
      taskId = resp.taskId
      onTaskCreated?.()
    } catch {
      // Task creation failed, continue without task tracking
    }

    let fileIndex = 0
    let aborted = false
    for (let i = 0; i < items.length; i++) {
      if (items[i].status !== 'pending') continue

      setItems((prev) => prev.map((item, idx) =>
        idx === i ? { ...item, status: 'uploading' as const } : item
      ))

      try {
        await api.uploadFile(
          vaultId,
          currentPath,
          items[i].file,
          (pct) => {
            setItems((prev) => prev.map((item, idx) =>
              idx === i ? { ...item, progress: pct } : item
            ))
          },
          taskId,
          fileIndex,
          pendingItems.length,
          controller.signal,
        )
        setItems((prev) => prev.map((item, idx) =>
          idx === i ? { ...item, status: 'done' as const, progress: 100 } : item
        ))
        onUploaded()
      } catch (err) {
        if (err instanceof DOMException && err.name === 'AbortError') {
          aborted = true
          break
        }
        setItems((prev) => prev.map((item, idx) =>
          idx === i ? { ...item, status: 'error' as const, error: err instanceof Error ? err.message : '上传失败' } : item
        ))
      }
      fileIndex++
    }
    uploadAbortRef.current = null
    setUploading(false)
    if (!aborted) onUploaded()
  }

  const pendingCount = items.filter((i) => i.status === 'pending').length
  const doneCount = items.filter((i) => i.status === 'done').length
  const allDone = items.length > 0 && doneCount === items.length

  return (
    <div className="fixed inset-0 bg-black/60 flex items-center justify-center z-50 p-2 sm:p-4" onClick={handleClose}>
      <div className="bg-gray-900 border border-gray-800 rounded-2xl p-6 w-full max-w-lg max-h-[calc(100vh-2rem)] sm:max-h-[80vh] flex flex-col" onClick={(e) => e.stopPropagation()}>
        <div className="flex items-center justify-between mb-4">
          <h2 className="text-lg font-semibold text-white">上传文件</h2>
          <button onClick={handleClose} className="p-1 text-gray-400 hover:text-white"><X className="w-5 h-5" /></button>
        </div>

        <div
          className={`border-2 border-dashed rounded-xl p-8 text-center transition-colors mb-4 cursor-pointer ${
            dragOver ? 'border-blue-500 bg-blue-500/10' : 'border-gray-700 hover:border-gray-600'
          }`}
          onClick={() => inputRef.current?.click()}
          onDragOver={(e) => { e.preventDefault(); setDragOver(true) }}
          onDragLeave={() => setDragOver(false)}
          onDrop={handleDrop}
        >
          <Upload className="w-8 h-8 text-gray-500 mx-auto mb-2" />
          <p className="text-gray-400 text-sm">拖拽文件到这里，或点击选择</p>
          <input ref={inputRef} type="file" multiple className="hidden" onChange={(e) => addFiles(e.target.files)} />
        </div>

        {items.length > 0 && (
          <div className="flex-1 overflow-y-auto space-y-2 mb-4 min-h-0">
            {items.map((item, i) => (
              <div key={i} className="bg-gray-800 rounded-lg p-3">
                <div className="flex items-center gap-2 mb-1">
                  {item.status === 'done' ? <CheckCircle2 className="w-4 h-4 text-green-500 flex-shrink-0" /> :
                   item.status === 'error' ? <AlertCircle className="w-4 h-4 text-red-500 flex-shrink-0" /> :
                   <FileUp className="w-4 h-4 text-gray-400 flex-shrink-0" />}
                  <span className="text-sm text-gray-300 truncate flex-1">{item.file.name}</span>
                  <span className="text-xs text-gray-500 flex-shrink-0">{formatSize(item.file.size)}</span>
                </div>
                {(item.status === 'uploading' || item.status === 'done') && (
                  <div className="h-1 bg-gray-700 rounded-full overflow-hidden">
                    <div
                      className={`h-full rounded-full transition-all ${item.status === 'done' ? 'bg-green-500' : 'bg-blue-500'}`}
                      style={{ width: `${item.progress}%` }}
                    />
                  </div>
                )}
                {item.status === 'error' && <p className="text-xs text-red-400 mt-1">{item.error}</p>}
              </div>
            ))}
          </div>
        )}

        <div className="flex gap-3">
          <button onClick={handleClose}
            className="flex-1 bg-gray-800 hover:bg-gray-700 text-gray-300 py-2.5 rounded-xl font-medium transition-colors">
            {allDone ? '关闭' : '取消'}
          </button>
          {!allDone && (
            <button
              onClick={uploadAll}
              disabled={pendingCount === 0 || uploading}
              className="flex-1 bg-blue-600 hover:bg-blue-700 disabled:opacity-50 text-white py-2.5 rounded-xl font-medium transition-colors"
            >
              {uploading ? '上传中...' : `上传 (${pendingCount})`}
            </button>
          )}
        </div>
      </div>
    </div>
  )
}
