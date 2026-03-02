import { useState } from 'react'
import { PhotoView } from 'react-photo-view'
import { api, type FileItem, isImage, isVideo, formatSize, joinPath } from '../lib/api'
import { useImagePreviewSrc } from '../lib/useImagePreviewSrc'
import { Folder, File, Film, Image, Music, Trash2, Loader2 } from 'lucide-react'

function VideoThumb({ src, alt }: { src: string; alt: string }) {
  const [err, setErr] = useState(false)
  if (err) return <Film className="w-10 h-10 text-purple-500" />
  return <img src={src} alt={alt} className="w-full h-full object-cover" onError={() => setErr(true)} />
}

export default function FileGridItem({ file, vaultId, currentPath, onOpenDir, onPlayVideo, onDelete }: {
  file: FileItem
  vaultId: string
  currentPath: string
  onOpenDir: (name: string) => void
  onPlayVideo: (url: string, title: string) => void
  onDelete: (file: FileItem) => void
}) {
  const filePath = joinPath(currentPath, file.name)
  const contentUrl = api.getContentUrl(vaultId, filePath)
  const { src: previewUrl, status: previewStatus, onPreviewError } = useImagePreviewSrc(file.name, contentUrl)

  if (file.isDir) {
    return (
      <button onClick={() => onOpenDir(file.name)}
        className="bg-gray-900 border border-gray-800 hover:border-gray-700 rounded-xl p-4 flex flex-col items-center gap-2 transition-all group text-center relative">
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
    if (previewStatus !== 'ready' || !previewUrl) {
      return (
        <div className="relative group">
          <div className="bg-gray-900 border border-gray-800 rounded-xl overflow-hidden">
            <div className="aspect-square bg-gray-800 flex items-center justify-center">
              {previewStatus === 'loading' ? (
                <div className="flex flex-col items-center gap-2 text-gray-400">
                  <Loader2 className="w-5 h-5 animate-spin" />
                  <span className="text-xs">加载中...</span>
                </div>
              ) : (
                <div className="flex flex-col items-center gap-2 text-gray-500">
                  <Image className="w-6 h-6" />
                  <span className="text-xs">暂不支持预览</span>
                </div>
              )}
            </div>
            <div className="p-2">
              <p className="text-xs text-gray-400 truncate">{file.name}</p>
            </div>
          </div>
          <button onClick={() => onDelete(file)}
            className="absolute top-2 right-2 p-1 bg-black/50 rounded text-gray-400 hover:text-red-400 sm:opacity-0 sm:group-hover:opacity-100 transition-all z-10">
            <Trash2 className="w-3.5 h-3.5" />
          </button>
        </div>
      )
    }

    return (
      <div className="relative group">
        <PhotoView src={previewUrl}>
          <div className="bg-gray-900 border border-gray-800 hover:border-gray-700 rounded-xl overflow-hidden cursor-pointer transition-all">
            <div className="aspect-square bg-gray-800">
              <img src={previewUrl} alt={file.name} className="w-full h-full object-cover" loading="lazy" onError={onPreviewError} />
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
    const thumbUrl = file.hasThumb ? api.getThumbnailUrl(vaultId, filePath) : ''
    return (
      <div className="relative group">
        <button onClick={() => onPlayVideo(contentUrl, file.name)}
          className="w-full bg-gray-900 border border-gray-800 hover:border-gray-700 rounded-xl overflow-hidden transition-all text-center">
          <div className="aspect-video bg-gray-800 flex items-center justify-center">
            {thumbUrl ? <VideoThumb src={thumbUrl} alt={file.name} /> : <Film className="w-10 h-10 text-purple-500" />}
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
