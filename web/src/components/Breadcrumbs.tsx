import { Home, ChevronRight } from 'lucide-react'

interface BreadcrumbsProps {
  path: string
  onNavigate: (path: string) => void
  rootPath?: string
  rootLabel?: string
}

export default function Breadcrumbs({ path, onNavigate, rootPath = '/', rootLabel }: BreadcrumbsProps) {
  const pathParts = path.split('/').filter(Boolean)

  return (
    <nav className="flex items-center gap-1 text-sm overflow-x-auto flex-1 min-w-0">
      <button
        onClick={() => onNavigate(rootPath)}
        className="text-gray-400 hover:text-white transition-colors flex-shrink-0"
        title={rootLabel || 'Root'}
      >
        <Home className="w-4 h-4" />
      </button>
      {pathParts.map((part, i) => {
        const partPath = rootPath === '/' 
          ? '/' + pathParts.slice(0, i + 1).join('/')
          : rootPath + '/' + pathParts.slice(0, i + 1).join('/')
        return (
          <span key={i} className="flex items-center gap-1 flex-shrink-0">
            <ChevronRight className="w-3 h-3 text-gray-600" />
            <button
              onClick={() => onNavigate(partPath)}
              className={`hover:text-white transition-colors ${i === pathParts.length - 1 ? 'text-white font-medium' : 'text-gray-400'}`}
            >
              {part}
            </button>
          </span>
        )
      })}
    </nav>
  )
}