import { useState } from 'react'
import { PhotoView } from 'react-photo-view'
import { api, type FileItem, isImage, isVideo, formatSize, joinPath } from '../lib/api'
import { useImagePreviewSrc } from '../lib/useImagePreviewSrc'
import ManagedImage from './ManagedImage'
import { Folder, File, Film, Image, Music, Trash2, Loader2, Download } from 'lucide-react'

function VideoThumb({ src, alt }: { src: string; alt: string }) {
  const [failedSrc, setFailedSrc] = useState<string | null>(null)
  if (failedSrc === src) return <Film className="w-10 h-10 text-purple-500" />
  return <ManagedImage src={src} alt={alt} className="w-full h-full object-cover" onError={() => setFailedSrc(src)} />
}

export default function FileGridItem({ file, vaultId, currentPath, onOpenDir, onPlayVideo, onDelete, onDownload }: {
  file: FileItem
  vaultId: string
  currentPath: string
  onOpenDir: (name: string) => void
  onPlayVideo: (url: string, title: string) => void
  onDelete: (file: FileItem) => void
  onDownload: (file: FileItem) => void
}) {
  const filePath = joinPath(currentPath, file.name)
  const contentUrl = api.getContentUrl(vaultId, filePath)
  const videoUrl = api.getVideoUrl(vaultId, filePath)
  const thumbnailUrl = api.getThumbnailUrl(vaultId, filePath)
  const imageFile = !file.isDir && isImage(file.name)
  const {
    src: previewUrl,
    status: previewStatus,
    onPreviewError,
    previewRef,
  } = useImagePreviewSrc(file.name, contentUrl, thumbnailUrl, {
    enabled: imageFile,
    lazy: imageFile,
  })

  if (file.isDir) {
    return (
      <div onClick={() => onOpenDir(file.name)}
        className="bg-gray-900 border border-gray-800 hover:border-gray-700 rounded-xl p-4 flex flex-col items-center gap-2 transition-all group text-center relative cursor-pointer">
        <Folder className="w-10 h-10 text-amber-500" />
        <span className="text-sm text-gray-300 truncate w-full">{file.name}</span>
        <div className="absolute top-2 right-2 flex gap-1 sm:opacity-0 sm:group-hover:opacity-100 transition-all">
          <button onClick={(e) => { e.stopPropagation(); onDownload(file) }}
            className="p-1 text-gray-600 hover:text-blue-400">
            <Download className="w-3.5 h-3.5" />
          </button>
          <button onClick={(e) => { e.stopPropagation(); onDelete(file) }}
            className="p-1 text-gray-600 hover:text-red-400">
            <Trash2 className="w-3.5 h-3.5" />
          </button>
        </div>
      </div>
    )
  }

  if (isImage(file.name)) {
    if (previewStatus !== 'ready' || !previewUrl) {
      return (
        <div ref={previewRef} className="relative group">
          <div className="bg-gray-900 border border-gray-800 rounded-xl overflow-hidden">
            <div className="aspect-square bg-gray-800 flex items-center justify-center">
              {previewStatus === 'loading' ? (
                <div className="flex flex-col items-center gap-2 text-gray-400">
                  <Loader2 className="w-5 h-5 animate-spin" />
                  <span className="text-xs">加载中...</span>
                </div>
              ) : previewStatus === 'idle' ? (
                <div className="flex flex-col items-center gap-2 text-gray-500">
                  <Image className="w-6 h-6" />
                  <span className="text-xs">等待预览</span>
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
          <div className="absolute top-2 right-2 flex gap-1 sm:opacity-0 sm:group-hover:opacity-100 transition-all z-10">
            <button onClick={(e) => { e.stopPropagation(); onDownload(file) }}
              className="p-1 bg-black/50 rounded text-gray-400 hover:text-blue-400">
              <Download className="w-3.5 h-3.5" />
            </button>
            <button onClick={(e) => { e.stopPropagation(); onDelete(file) }}
              className="p-1 bg-black/50 rounded text-gray-400 hover:text-red-400">
              <Trash2 className="w-3.5 h-3.5" />
            </button>
          </div>
        </div>
      )
    }

    return (
      <div ref={previewRef} className="relative group">
        <PhotoView src={previewUrl}>
          <div className="bg-gray-900 border border-gray-800 hover:border-gray-700 rounded-xl overflow-hidden cursor-pointer transition-all">
            <div className="aspect-square bg-gray-800">
              <ManagedImage src={previewUrl} alt={file.name} className="w-full h-full object-cover" onError={onPreviewError} />
            </div>
            <div className="p-2">
              <p className="text-xs text-gray-400 truncate">{file.name}</p>
            </div>
          </div>
        </PhotoView>
        <div className="absolute top-2 right-2 flex gap-1 sm:opacity-0 sm:group-hover:opacity-100 transition-all z-10">
          <button onClick={(e) => { e.stopPropagation(); onDownload(file) }}
            className="p-1 bg-black/50 rounded text-gray-400 hover:text-blue-400">
            <Download className="w-3.5 h-3.5" />
          </button>
          <button onClick={(e) => { e.stopPropagation(); onDelete(file) }}
            className="p-1 bg-black/50 rounded text-gray-400 hover:text-red-400">
            <Trash2 className="w-3.5 h-3.5" />
          </button>
        </div>
      </div>
    )
  }

  if (isVideo(file.name)) {
    const thumbUrl = file.hasThumb ? thumbnailUrl : ''
    return (
      <div className="relative group">
        <div onClick={() => onPlayVideo(videoUrl, file.name)}
          className="w-full bg-gray-900 border border-gray-800 hover:border-gray-700 rounded-xl overflow-hidden transition-all text-center cursor-pointer">
          <div className="aspect-video bg-gray-800 flex items-center justify-center">
            {thumbUrl ? <VideoThumb src={thumbUrl} alt={file.name} /> : <Film className="w-10 h-10 text-purple-500" />}
          </div>
          <div className="p-2">
            <p className="text-xs text-gray-400 truncate">{file.name}</p>
            <p className="text-xs text-gray-500">{formatSize(file.size)}</p>
          </div>
        </div>
        <div className="absolute top-2 right-2 flex gap-1 sm:opacity-0 sm:group-hover:opacity-100 transition-all z-10">
          <button onClick={(e) => { e.stopPropagation(); onDownload(file) }}
            className="p-1 bg-black/50 rounded text-gray-400 hover:text-blue-400">
            <Download className="w-3.5 h-3.5" />
          </button>
          <button onClick={(e) => { e.stopPropagation(); onDelete(file) }}
            className="p-1 bg-black/50 rounded text-gray-400 hover:text-red-400">
            <Trash2 className="w-3.5 h-3.5" />
          </button>
        </div>
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
      <div className="absolute top-2 right-2 flex gap-1 sm:opacity-0 sm:group-hover:opacity-100 transition-all">
        <button onClick={(e) => { e.stopPropagation(); onDownload(file) }}
          className="p-1 text-gray-600 hover:text-blue-400">
          <Download className="w-3.5 h-3.5" />
        </button>
        <button onClick={(e) => { e.stopPropagation(); onDelete(file) }}
          className="p-1 text-gray-600 hover:text-red-400">
          <Trash2 className="w-3.5 h-3.5" />
        </button>
      </div>
    </div>
  )
}
