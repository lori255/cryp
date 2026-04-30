import { PhotoView } from 'react-photo-view'
import { api, type FileItem, isImage, isVideo, formatSize, formatDate, joinPath } from '../lib/api'
import { useImagePreviewSrc } from '../lib/useImagePreviewSrc'
import { Folder, File, Film, Image, Trash2, Download } from 'lucide-react'

export default function FileListItem({ file, vaultId, currentPath, onOpenDir, onPlayVideo, onDelete, onDownload }: {
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
  const { src: previewUrl } = useImagePreviewSrc(file.name, contentUrl, api.getThumbnailUrl(vaultId, filePath))

  function handleClick() {
    if (file.isDir) {
      onOpenDir(file.name)
    } else if (isVideo(file.name)) {
      onPlayVideo(contentUrl, file.name)
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
        onClick={(e) => { e.stopPropagation(); onDownload(file) }}
        className="p-1 text-gray-600 hover:text-blue-400 sm:opacity-0 sm:group-hover:opacity-100 transition-all flex-shrink-0"
      >
        <Download className="w-4 h-4" />
      </button>
      <button
        onClick={(e) => { e.stopPropagation(); onDelete(file) }}
        className="p-1 text-gray-600 hover:text-red-400 sm:opacity-0 sm:group-hover:opacity-100 transition-all flex-shrink-0"
      >
        <Trash2 className="w-4 h-4" />
      </button>
    </div>
  )

  if (isImage(file.name)) {
    return <PhotoView src={previewUrl}>{row}</PhotoView>
  }

  return row
}
