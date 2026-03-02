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
      autoplay: false,
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

      let primedInitialSeek = false
      const primeInitialSeek = () => {
        if (primedInitialSeek || videoEl.currentTime > 0) {
          primedInitialSeek = true
          return
        }

        const applyNudge = () => {
          if (primedInitialSeek || videoEl.currentTime > 0) {
            primedInitialSeek = true
            return
          }
          if (videoEl.readyState < HTMLMediaElement.HAVE_METADATA) {
            return
          }

          const duration = Number.isFinite(videoEl.duration) ? videoEl.duration : 0
          const targetTime = duration > 0 ? Math.min(0.001, duration) : 0.001
          videoEl.currentTime = targetTime
          primedInitialSeek = true
        }

        if (videoEl.readyState >= HTMLMediaElement.HAVE_METADATA) {
          applyNudge()
          return
        }

        videoEl.addEventListener('loadedmetadata', applyNudge, { once: true })
      }

      const onPlaying = () => {
        primedInitialSeek = true
      }

      videoEl.addEventListener('play', primeInitialSeek)
      videoEl.addEventListener('playing', onPlaying)

      art.on('destroy', () => {
        videoEl.removeEventListener('play', primeInitialSeek)
        videoEl.removeEventListener('playing', onPlaying)
      })
    }

    // On iOS PWA, native fullscreen API doesn't work.
    // Override fullscreen button to use webkitEnterFullscreen on the <video> element
    if (isIOS) {
      art.on('fullscreen', (state) => {
        const iosVideo = videoEl as HTMLVideoElement & {
          webkitEnterFullscreen?: () => void
        }
        if (state && iosVideo?.webkitEnterFullscreen) {
          iosVideo.webkitEnterFullscreen()
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
    const { style } = document.body
    const previousOverflow = style.overflow
    const previousPosition = style.position
    const previousTop = style.top
    const previousWidth = style.width
    const scrollY = window.scrollY

    style.overflow = 'hidden'
    style.position = 'fixed'
    style.top = `-${scrollY}px`
    style.width = '100%'

    return () => {
      style.overflow = previousOverflow
      style.position = previousPosition
      style.top = previousTop
      style.width = previousWidth
      window.scrollTo(0, scrollY)
    }
  }, [])

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
