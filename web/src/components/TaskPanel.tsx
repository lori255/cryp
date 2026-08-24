import { useCallback, useState, useEffect, useRef } from 'react'
import { api, type TaskRecord, formatSize, formatETA } from '../lib/api'
import { X, FolderInput, Upload, Trash2, XCircle, ListTodo, Loader2, ScanSearch } from 'lucide-react'

interface TaskPanelProps {
  vaultId: string
  open: boolean
  onClose: () => void
  onRefresh?: () => void
}

interface TaskActionError {
  taskId?: string
  message: string
}

export default function TaskPanel({ vaultId, open, onClose, onRefresh }: TaskPanelProps) {
  const [tasks, setTasks] = useState<TaskRecord[]>([])
  const [loading, setLoading] = useState(true)
  const [pollError, setPollError] = useState('')
  const [actionError, setActionError] = useState<TaskActionError | null>(null)
  const pollTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)
  const openRef = useRef(open)
  const prevTasksRef = useRef<TaskRecord[]>([])
  const pollAbortRef = useRef<AbortController | null>(null)
  const pollRequestIdRef = useRef(0)
  const mountedRef = useRef(true)
  const cancelPolling = useCallback(() => {
    pollRequestIdRef.current += 1
    pollAbortRef.current?.abort()
    pollAbortRef.current = null
    if (pollTimerRef.current) {
      clearTimeout(pollTimerRef.current)
      pollTimerRef.current = null
    }
  }, [])

  const loadTasks = useCallback(async () => {
    if (!mountedRef.current || !openRef.current) return
    if (pollAbortRef.current) return
    if (pollTimerRef.current) {
      clearTimeout(pollTimerRef.current)
      pollTimerRef.current = null
    }
    const requestId = ++pollRequestIdRef.current
    const controller = new AbortController()
    pollAbortRef.current = controller
    try {
      const data = await api.listTasks(vaultId, controller.signal)
      if (!mountedRef.current || requestId !== pollRequestIdRef.current) return
      const newTasks = data.tasks || []
      
      const prevTasks = prevTasksRef.current
      const hasCompletedTask = newTasks.some(newTask => {
        if (newTask.status === 'done') {
          const prevTask = prevTasks.find(t => t.id === newTask.id)
          return prevTask && prevTask.status === 'running'
        }
        return false
      })
      
      if (hasCompletedTask) {
        onRefresh?.()
      }

      prevTasksRef.current = newTasks
      setPollError('')
      setTasks(newTasks)
    } catch {
      if (!mountedRef.current || controller.signal.aborted || requestId !== pollRequestIdRef.current) return
      setPollError('任务列表加载失败')
    } finally {
      if (mountedRef.current && requestId === pollRequestIdRef.current) {
        pollAbortRef.current = null
        setLoading(false)
        if (openRef.current && !controller.signal.aborted) {
          const delay = document.visibilityState === 'hidden' ? 5000 : 2000
          pollTimerRef.current = setTimeout(() => void loadTasks(), delay)
        }
      }
    }
  }, [vaultId, onRefresh])

  useEffect(() => {
    mountedRef.current = true
    openRef.current = open
    cancelPolling()
    prevTasksRef.current = []
    if (open) {
      setLoading(true)
      setPollError('')
      setActionError(null)
      void loadTasks()
    }
    return () => {
      mountedRef.current = false
      openRef.current = false
      cancelPolling()
    }
  }, [open, loadTasks, cancelPolling])

  async function handleCancel(taskId: string) {
    setActionError(null)
    try {
      await api.cancelTask(vaultId, taskId)
      if (mountedRef.current) {
        void loadTasks()
      }
    } catch (err) {
      if (!mountedRef.current) return
      setActionError({ taskId, message: err instanceof Error ? err.message : '取消任务失败' })
    }
  }

  async function handleDelete(taskId: string) {
    setActionError(null)
    try {
      await api.deleteTask(vaultId, taskId)
      if (mountedRef.current) {
        void loadTasks()
      }
    } catch (err) {
      if (!mountedRef.current) return
      setActionError({ taskId, message: err instanceof Error ? err.message : '删除任务失败' })
    }
  }

  async function handleDeleteCompleted() {
    setActionError(null)
    try {
      await api.deleteCompletedTasks(vaultId)
      if (mountedRef.current) {
        void loadTasks()
      }
    } catch (err) {
      if (!mountedRef.current) return
      setActionError({ message: err instanceof Error ? err.message : '清除已完成任务失败' })
    }
  }

  if (!open) return null

  const hasRunning = tasks.some(t => t.status === 'running')

  return (
    <>
      <div className="fixed inset-0 bg-black/40 z-40" onClick={onClose} />
      <div className="fixed right-0 top-0 h-full w-full max-w-md bg-gray-950 border-l border-gray-800 z-50 flex flex-col animate-slide-in">
        <div className="flex items-center justify-between px-4 h-14 border-b border-gray-800 flex-shrink-0">
          <div className="flex items-center gap-2">
            <ListTodo className="w-5 h-5 text-blue-500" />
            <h2 className="text-lg font-semibold text-white">任务列表</h2>
            {hasRunning && <Loader2 className="w-4 h-4 text-blue-400 animate-spin" />}
          </div>
          <div className="flex items-center gap-2">
            {tasks.some(t => t.status === 'done') && (
              <button
                onClick={() => void handleDeleteCompleted()}
                className="flex items-center gap-1 px-2 py-1 text-xs text-gray-400 hover:text-red-400 transition-colors"
              >
                <Trash2 className="w-3.5 h-3.5" /> 清除已完成
              </button>
            )}
            <button onClick={onClose} className="p-1 text-gray-400 hover:text-white">
              <X className="w-5 h-5" />
            </button>
          </div>
        </div>

        <div className="flex-1 overflow-y-auto p-4 space-y-3">
          {actionError && (
            <p className="text-xs text-red-400 bg-red-500/10 rounded-lg px-3 py-2">
              {actionError.message}
            </p>
          )}
          {pollError && !actionError && (
            <p className="text-xs text-amber-400 bg-amber-500/10 rounded-lg px-3 py-2">
              {pollError}
            </p>
          )}
          {loading ? (
            <div className="flex items-center justify-center py-20">
              <div className="animate-spin rounded-full h-6 w-6 border-2 border-blue-500 border-t-transparent" />
            </div>
          ) : tasks.length === 0 ? (
            <div className="text-center py-20">
              <ListTodo className="w-10 h-10 text-gray-700 mx-auto mb-3" />
              <p className="text-gray-500 text-sm">暂无任务</p>
            </div>
          ) : (
            tasks.map(t => <TaskItem key={t.id} task={t} onCancel={handleCancel} onDelete={handleDelete} />)
          )}
        </div>
      </div>
    </>
  )
}

function TaskItem({ task: t, onCancel, onDelete }: {
  task: TaskRecord
  onCancel: (id: string) => void
  onDelete: (id: string) => void
}) {
  const pct = t.totalFiles > 0
    ? Math.min(100, Math.max(0, Math.round((t.processedFiles / t.totalFiles) * 100)))
    : 0
  const isActive = t.status === 'running'
  const hasKnownTotal = t.totalFiles > 0

  const statusConfig: Record<TaskRecord['status'], { label: string; color: string }> = {
    pending: { label: '等待中', color: 'bg-yellow-500/20 text-yellow-400' },
    running: { label: '运行中', color: 'bg-blue-500/20 text-blue-400' },
    done: { label: '已完成', color: 'bg-green-500/20 text-green-400' },
    error: { label: '错误', color: 'bg-red-500/20 text-red-400' },
    cancelled: { label: '已取消', color: 'bg-gray-500/20 text-gray-400' },
  }

  const status = statusConfig[t.status]

  return (
    <div className="bg-gray-900 border border-gray-800 rounded-xl p-4 space-y-3">
      <div className="flex items-start justify-between">
        <div className="flex items-center gap-2.5">
          {t.type === 'import'
            ? <FolderInput className="w-5 h-5 text-amber-500 flex-shrink-0" />
            : t.type === 'index'
              ? <ScanSearch className="w-5 h-5 text-cyan-400 flex-shrink-0" />
              : <Upload className="w-5 h-5 text-blue-500 flex-shrink-0" />}
          <div>
            <p className="text-sm font-medium text-white">
              {t.type === 'import' ? '导入加密' : t.type === 'index' ? '重建索引' : '文件上传'}
            </p>
            {t.sourcePath && (
              <p className="text-xs text-gray-500 truncate max-w-[240px]">{t.sourcePath}</p>
            )}
          </div>
        </div>
        <span className={`text-xs px-2 py-0.5 rounded-full font-medium ${status.color}`}>
          {status.label}
        </span>
      </div>

      {(isActive || t.status === 'done') && hasKnownTotal && (
        <div>
          <div className="flex justify-between text-xs text-gray-400 mb-1">
            <span>已处理 {t.processedFiles}/{t.totalFiles} 文件</span>
            <span>{pct}%</span>
          </div>
          <div className="h-1.5 bg-gray-800 rounded-full overflow-hidden">
            <div
              className={`h-full rounded-full transition-all duration-300 ${t.status === 'done' ? 'bg-green-500' : 'bg-blue-500'}`}
              style={{ width: `${pct}%` }}
            />
          </div>
          <div className="flex justify-between text-xs text-gray-500 mt-1">
            <span>{formatSize(t.processedBytes)} / {formatSize(t.totalBytes)}</span>
            {isActive && t.startedAt > 0 && t.processedBytes > 0 && (
              <span>{formatETA(t.startedAt, t.processedBytes, t.totalBytes)}</span>
            )}
          </div>
        </div>
      )}

      {(isActive || t.status === 'done') && !hasKnownTotal && (
        <div className="flex justify-between text-xs text-gray-500">
          <span>已处理 {t.processedFiles} 个文件</span>
          <span>{formatSize(t.processedBytes)}</span>
        </div>
      )}

      {isActive && t.currentFile && (
        <p className="text-xs text-gray-500 truncate">
          当前: {t.currentFile}
        </p>
      )}

      {t.status === 'error' && t.errorMsg && (
        <p className="text-xs text-red-400 bg-red-500/10 rounded-lg px-3 py-2">{t.errorMsg}</p>
      )}

      <div className="flex justify-end gap-2">
        {isActive && (
          <button
            onClick={() => onCancel(t.id)}
            className="flex items-center gap-1 text-xs text-gray-400 hover:text-red-400 transition-colors"
          >
            <XCircle className="w-3.5 h-3.5" /> 取消
          </button>
        )}
        {(t.status === 'done' || t.status === 'error' || t.status === 'cancelled') && (
          <button
            onClick={() => onDelete(t.id)}
            className="flex items-center gap-1 text-xs text-gray-400 hover:text-red-400 transition-colors"
          >
            <Trash2 className="w-3.5 h-3.5" /> 删除
          </button>
        )}
      </div>
    </div>
  )
}
