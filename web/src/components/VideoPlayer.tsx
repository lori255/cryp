import { useEffect, useRef } from 'react'
import Artplayer from 'artplayer'
import { X } from 'lucide-react'

interface VideoPlayerProps {
  url: string
  title: string
  onClose: () => void
}

export default function VideoPlayer({ url, title, onClose }: VideoPlayerProps) {
  const containerRef = useRef<HTMLDivElement>(null)
  const artRef = useRef<Artplayer | null>(null)

  useEffect(() => {
    if (!containerRef.current) return

    const isIOS = /iPad|iPhone|iPod/.test(navigator.userAgent)

    const art = new Artplayer({
      container: containerRef.current,
      url,
      volume: 0.7,
      isLive: false,
      muted: false,
      autoplay: true,
      pip: !isIOS,
      autoSize: true,
      autoMini: false,
      screenshot: !isIOS,
      setting: true,
      loop: false,
      flip: true,
      playbackRate: true,
      aspectRatio: true,
      fullscreen: true,
      fullscreenWeb: true,
      mutex: true,
      backdrop: true,
      hotkey: true,
      theme: '#3b82f6',
      customType: {
        // Ensure playsinline for iOS
        m3u8: function (video: HTMLVideoElement) {
          video.setAttribute('playsinline', '')
          video.setAttribute('webkit-playsinline', '')
        },
      },
    })

    // Set playsinline on the video element for iOS
    const videoEl = art.video
    if (videoEl) {
      videoEl.setAttribute('playsinline', '')
      videoEl.setAttribute('webkit-playsinline', '')
    }

    // On iOS PWA, native fullscreen API doesn't work.
    // Override fullscreen button to use webkitEnterFullscreen on the <video> element
    if (isIOS) {
      art.on('fullscreen', (state) => {
        if (state && videoEl && 'webkitEnterFullscreen' in videoEl) {
          ;(videoEl as any).webkitEnterFullscreen()
        }
      })
    }

    artRef.current = art

    return () => {
      if (artRef.current) {
        artRef.current.destroy(false)
        artRef.current = null
      }
    }
  }, [url])

  useEffect(() => {
    function handleKeyDown(e: KeyboardEvent) {
      if (e.key === 'Escape') {
        onClose()
      }
    }
    window.addEventListener('keydown', handleKeyDown)
    return () => window.removeEventListener('keydown', handleKeyDown)
  }, [onClose])

  return (
    <div className="fixed inset-0 bg-black/90 z-50 flex flex-col" onClick={onClose}>
      {/* Title bar */}
      <div className="flex items-center justify-between px-4 py-3 flex-shrink-0" onClick={(e) => e.stopPropagation()}>
        <h3 className="text-white font-medium truncate">{title}</h3>
        <button onClick={onClose} className="p-2 text-gray-400 hover:text-white transition-colors">
          <X className="w-6 h-6" />
        </button>
      </div>

      {/* Player */}
      <div className="flex-1 flex items-center justify-center px-4 pb-4" onClick={(e) => e.stopPropagation()}>
        <div className="w-full max-w-6xl h-[60vh] sm:h-[75vh] touch-manipulation">
          <div ref={containerRef} className="w-full h-full" />
        </div>
      </div>
    </div>
  )
}
