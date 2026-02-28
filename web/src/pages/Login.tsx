import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { api } from '../lib/api'
import { Lock, Plus, Shield, ArrowLeft } from 'lucide-react'

export default function Login() {
  const navigate = useNavigate()
  const [mode, setMode] = useState<'login' | 'create'>('login')
  const [name, setName] = useState('')
  const [password, setPassword] = useState('')
  const [confirmPassword, setConfirmPassword] = useState('')
  const [error, setError] = useState('')
  const [submitting, setSubmitting] = useState(false)

  async function handleLogin(e: React.FormEvent) {
    e.preventDefault()
    setError('')
    setSubmitting(true)
    try {
      const data = await api.login(name, password)
      navigate(`/vault/${data.vaultId}`)
    } catch (err) {
      setError(err instanceof Error ? err.message : '登录失败')
    } finally {
      setSubmitting(false)
    }
  }

  async function handleCreate(e: React.FormEvent) {
    e.preventDefault()
    if (password !== confirmPassword) {
      setError('两次密码不一致')
      return
    }
    if (password.length < 4) {
      setError('密码至少4位')
      return
    }
    setError('')
    setSubmitting(true)
    try {
      const data = await api.createVault(name, password)
      navigate(`/vault/${data.id}`)
    } catch (err) {
      setError(err instanceof Error ? err.message : '创建失败')
    } finally {
      setSubmitting(false)
    }
  }

  function switchMode(newMode: 'login' | 'create') {
    setMode(newMode)
    setError('')
    setName('')
    setPassword('')
    setConfirmPassword('')
  }

  return (
    <div className="min-h-screen flex items-center justify-center p-4">
      <div className="w-full max-w-md">
        {/* Logo */}
        <div className="text-center mb-8">
          <div className="inline-flex items-center justify-center w-16 h-16 rounded-2xl bg-blue-600/20 mb-4">
            <Shield className="w-8 h-8 text-blue-500" />
          </div>
          <h1 className="text-3xl font-bold text-white">Cryp</h1>
          <p className="text-gray-400 mt-2">加密文件保险库</p>
        </div>

        {/* Login Form */}
        {mode === 'login' && (
          <div className="bg-gray-900 rounded-2xl p-8 border border-gray-800">
            <div className="flex items-center gap-3 mb-6">
              <div className="w-10 h-10 rounded-xl bg-blue-600/20 flex items-center justify-center">
                <Lock className="w-5 h-5 text-blue-500" />
              </div>
              <div>
                <h2 className="text-lg font-semibold text-white">解锁保险库</h2>
                <p className="text-gray-500 text-sm">输入保险库名称和密码</p>
              </div>
            </div>
            <form onSubmit={handleLogin} className="space-y-4">
              <div>
                <label className="block text-sm text-gray-400 mb-1">保险库名称</label>
                <input
                  type="text"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  className="w-full bg-gray-800 border border-gray-700 rounded-xl px-4 py-3 text-white placeholder-gray-500 focus:outline-none focus:ring-2 focus:ring-blue-500 focus:border-transparent"
                  placeholder="输入保险库名称"
                  autoFocus
                  required
                />
              </div>
              <div>
                <label className="block text-sm text-gray-400 mb-1">密码</label>
                <input
                  type="password"
                  value={password}
                  onChange={(e) => setPassword(e.target.value)}
                  className="w-full bg-gray-800 border border-gray-700 rounded-xl px-4 py-3 text-white placeholder-gray-500 focus:outline-none focus:ring-2 focus:ring-blue-500 focus:border-transparent"
                  placeholder="输入加密密码"
                  required
                />
              </div>
              {error && <p className="text-red-400 text-sm">{error}</p>}
              <button
                type="submit"
                disabled={submitting}
                className="w-full bg-blue-600 hover:bg-blue-700 disabled:opacity-50 text-white py-3 px-4 rounded-xl font-medium transition-colors"
              >
                {submitting ? '解锁中...' : '解锁'}
              </button>
            </form>
            <div className="mt-6 pt-4 border-t border-gray-800 text-center">
              <button
                onClick={() => switchMode('create')}
                className="text-gray-400 hover:text-blue-400 text-sm font-medium transition-colors inline-flex items-center gap-1.5"
              >
                <Plus className="w-4 h-4" /> 创建新保险库
              </button>
            </div>
          </div>
        )}

        {/* Create Vault Form */}
        {mode === 'create' && (
          <div className="bg-gray-900 rounded-2xl p-8 border border-gray-800">
            <div className="flex items-center gap-3 mb-6">
              <div className="w-10 h-10 rounded-xl bg-green-600/20 flex items-center justify-center">
                <Plus className="w-5 h-5 text-green-500" />
              </div>
              <div>
                <h2 className="text-lg font-semibold text-white">创建保险库</h2>
                <p className="text-gray-500 text-sm">设置名称和加密密码</p>
              </div>
            </div>
            <form onSubmit={handleCreate} className="space-y-4">
              <div>
                <label className="block text-sm text-gray-400 mb-1">保险库名称</label>
                <input
                  type="text"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  className="w-full bg-gray-800 border border-gray-700 rounded-xl px-4 py-3 text-white placeholder-gray-500 focus:outline-none focus:ring-2 focus:ring-blue-500 focus:border-transparent"
                  placeholder="例如：我的照片"
                  autoFocus
                  required
                />
              </div>
              <div>
                <label className="block text-sm text-gray-400 mb-1">加密密码</label>
                <input
                  type="password"
                  value={password}
                  onChange={(e) => setPassword(e.target.value)}
                  className="w-full bg-gray-800 border border-gray-700 rounded-xl px-4 py-3 text-white placeholder-gray-500 focus:outline-none focus:ring-2 focus:ring-blue-500 focus:border-transparent"
                  placeholder="输入加密密码"
                  required
                />
              </div>
              <div>
                <label className="block text-sm text-gray-400 mb-1">确认密码</label>
                <input
                  type="password"
                  value={confirmPassword}
                  onChange={(e) => setConfirmPassword(e.target.value)}
                  className="w-full bg-gray-800 border border-gray-700 rounded-xl px-4 py-3 text-white placeholder-gray-500 focus:outline-none focus:ring-2 focus:ring-blue-500 focus:border-transparent"
                  placeholder="再次输入密码"
                  required
                />
              </div>
              {error && <p className="text-red-400 text-sm">{error}</p>}
              <div className="flex gap-3 pt-2">
                <button
                  type="button"
                  onClick={() => switchMode('login')}
                  className="flex-1 bg-gray-800 hover:bg-gray-700 text-gray-300 py-3 px-4 rounded-xl font-medium transition-colors inline-flex items-center justify-center gap-1.5"
                >
                  <ArrowLeft className="w-4 h-4" /> 返回登录
                </button>
                <button
                  type="submit"
                  disabled={submitting}
                  className="flex-1 bg-blue-600 hover:bg-blue-700 disabled:opacity-50 text-white py-3 px-4 rounded-xl font-medium transition-colors"
                >
                  {submitting ? '创建中...' : '创建'}
                </button>
              </div>
            </form>
          </div>
        )}
      </div>
    </div>
  )
}
