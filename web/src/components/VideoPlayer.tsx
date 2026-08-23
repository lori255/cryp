import { useCallback, useEffect, useRef, useState } from 'react'
import Artplayer from 'artplayer'
import Hls from 'hls.js'
import { AlertCircle, Maximize, RefreshCw, X } from 'lucide-react'
import { api } from '../lib/api'

interface VideoPlayerProps {
  url: string
  title: string
  onClose: () => void
}

export default function VideoPlayer({ url, title, onClose }: VideoPlayerProps) {
  const [sourceState, setSourceState] = useState({ baseUrl: url, url })
  const [errorState, setErrorState] = useState<{ baseUrl: string; message: string } | null>(null)
  const [retryNonce, setRetryNonce] = useState(0)
  // When the parent changes url, ignore a fallback retained for the previous
  // item without needing a setState call inside an effect.
  const currentUrl = sourceState.baseUrl === url ? sourceState.url : url
  const error = errorState?.baseUrl === url ? errorState.message : ''
  const containerRef = useRef<HTMLDivElement>(null)
  const artRef = useRef<Artplayer | null>(null)
  const videoRef = useRef<HTMLVideoElement | null>(null)
  const unlockOrientationRef = useRef<(() => void) | null>(null)
  const stopHlsRef = useRef<() => void>(() => {})

  const lockLandscapeOrientation = useCallback(async () => {
    const orientationApi = screen.orientation as ScreenOrientation & {
      lock?: (orientation: 'landscape' | 'portrait' | 'any' | 'natural') => Promise<void>
      unlock?: () => void
    }
    if (!orientationApi?.lock) return null

    try {
      await orientationApi.lock('landscape')
      return () => orientationApi.unlock?.()
    } catch {
      return null
    }
  }, [])

  const requestNativeFullscreen = useCallback(async (video: HTMLVideoElement | null) => {
    if (!video) return

    const nativeVideo = video as HTMLVideoElement & {
      webkitSupportsFullscreen?: boolean
      webkitEnterFullscreen?: () => void
      webkitSetPresentationMode?: (mode: 'inline' | 'fullscreen' | 'picture-in-picture') => void
    }

    unlockOrientationRef.current?.()
    const unlockOrientation = await lockLandscapeOrientation()
    if (videoRef.current !== video) {
      unlockOrientation?.()
      return
    }
    unlockOrientationRef.current = unlockOrientation

    if (nativeVideo.webkitSupportsFullscreen && nativeVideo.webkitEnterFullscreen) {
      nativeVideo.webkitEnterFullscreen()
      return
    }

    if (nativeVideo.webkitSetPresentationMode) {
      try {
        nativeVideo.webkitSetPresentationMode('fullscreen')
        return
      } catch {
        // Fall through to the standard API.
      }
    }

    if (video.requestFullscreen) {
      void video.requestFullscreen().catch(() => {})
    }
  }, [lockLandscapeOrientation])

  useEffect(() => {
    if (!containerRef.current) return

    const activeUrl = currentUrl
    const activeBaseUrl = url
    let disposed = false
    let streamStopped = false
    let activeHlsUrl = activeUrl
    const probeController = new AbortController()
    const stopStream = () => {
      if (streamStopped) return
      streamStopped = true
      api.stopHls(activeHlsUrl)
    }
    stopHlsRef.current = stopStream

    const isIOS = /iPad|iPhone|iPod/.test(navigator.userAgent)
    const hlsInstances: Hls[] = []
    const destroyHlsInstances = () => {
      while (hlsInstances.length > 0) {
        hlsInstances.pop()?.destroy()
      }
    }

    const art = new Artplayer({
      container: containerRef.current,
      url: activeUrl,
      type: activeUrl.includes('/files/hls') || activeUrl.includes('.m3u8') ? 'm3u8' : undefined,
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
      fullscreen: !isIOS,
      fullscreenWeb: !isIOS,
      mutex: true,
      backdrop: true,
      hotkey: true,
      theme: '#3b82f6',
      autoOrientation: isIOS,
      controls: isIOS ? [{
        name: 'ios-native-fullscreen',
        position: 'right',
        index: 20,
        tooltip: '原生全屏',
        html: '<div class="art-icon"><svg viewBox="0 0 24 24" width="22" height="22" aria-hidden="true"><path fill="currentColor" d="M7 3H3v4h2V5h2V3zm14 0h-4v2h2v2h2V3zM5 17H3v4h4v-2H5v-2zm16 0h-2v2h-2v2h4v-4z"/></svg></div>',
        click: () => requestNativeFullscreen(videoRef.current),
        mounted: (element: HTMLElement) => {
          const nativeVideo = videoRef.current as (HTMLVideoElement & {
            webkitSupportsFullscreen?: boolean
            webkitEnterFullscreen?: () => void
          }) | null

          if (!nativeVideo?.webkitEnterFullscreen && !videoRef.current?.requestFullscreen) {
            element.style.display = 'none'
          }
        },
      }] : [],
      customType: {
        m3u8: function (video: HTMLVideoElement, sourceUrl: string) {
          video.setAttribute('playsinline', '')
          video.setAttribute('webkit-playsinline', '')

          if (Hls.isSupported()) {
            const hls = new Hls({
              enableWorker: true,
              lowLatencyMode: false,
              backBufferLength: 30,
              manifestLoadingMaxRetry: 2,
              manifestLoadingRetryDelay: 1000,
              manifestLoadingMaxRetryTimeout: 5000,
              levelLoadingMaxRetry: 2,
              levelLoadingRetryDelay: 1000,
              fragLoadingMaxRetry: 3,
              fragLoadingRetryDelay: 1000,
            })
            hls.loadSource(sourceUrl)
            hls.attachMedia(video)
            hlsInstances.push(hls)
            hls.on(Hls.Events.MANIFEST_LOADED, (_, data) => {
              const candidates = [data.networkDetails?.responseURL, data.url, sourceUrl]
              for (const candidate of candidates) {
                if (!candidate) continue
                try {
                  const candidatePath = new URL(candidate, window.location.origin).pathname
                  if (candidatePath.endsWith('/index.m3u8') || candidatePath.endsWith('/files/hls')) {
                    activeHlsUrl = candidate
                    break
                  }
                } catch {
                  // Keep the previously known source when an event payload
                  // contains a malformed/non-URL value.
                }
              }
            })
            let mediaRecoveryCount = 0
            hls.on(Hls.Events.ERROR, (_, data) => {
              if (disposed || !data.fatal) return

              // A single media recovery handles transient decoder state. Do
              // not retry fatal network errors indefinitely: each retry can
              // otherwise create another slow backend HLS startup.
              if (data.type === 'mediaError' && mediaRecoveryCount < 1) {
                mediaRecoveryCount++
                hls.recoverMediaError()
                return
              }

              stopStream()
              destroyHlsInstances()
              const detail = data as typeof data & {
                response?: { code?: number }
                networkDetails?: { status?: number }
              }
              const status = detail.response?.code ?? detail.networkDetails?.status
              let message = '视频转码播放失败'
              if (status === 401 || status === 403) {
                message = '登录状态已失效，请重新登录'
              } else if (status === 404) {
                message = '视频或转码分片不存在'
              } else if (status === 429) {
                message = '转码资源繁忙，请稍后重试'
              } else if (typeof status === 'number' && status >= 500) {
                message = '转码服务暂时不可用，请重试'
              }
              setErrorState({ baseUrl: activeBaseUrl, message })
            })
            return
          }

          if (video.canPlayType('application/vnd.apple.mpegurl')) {
            video.src = sourceUrl
          }
        },
      },
    })

    // Set playsinline on the video element for iOS
    const videoEl = art.video
    videoRef.current = videoEl
    if (videoEl) {
      videoEl.setAttribute('playsinline', '')
      videoEl.setAttribute('webkit-playsinline', '')
      videoEl.playsInline = true

      const releaseOrientationLock = () => {
        unlockOrientationRef.current?.()
        unlockOrientationRef.current = null
      }
      const handleFullscreenChange = () => {
        if (!document.fullscreenElement) {
          releaseOrientationLock()
        }
      }
      let stallCheckTimer: number | null = null
      let hasRecoveredInitialStall = false
      let hasTriedHlsFallback = activeUrl.includes('/files/hls')
      let contentFallbackInProgress = false

      const clearStallCheck = () => {
        if (stallCheckTimer !== null) {
          window.clearTimeout(stallCheckTimer)
          stallCheckTimer = null
        }
      }

      const scheduleInitialStallRecovery = () => {
        if (hasRecoveredInitialStall || videoEl.currentTime > 0) return

        clearStallCheck()
        stallCheckTimer = window.setTimeout(() => {
          stallCheckTimer = null

          if (hasRecoveredInitialStall || videoEl.paused || videoEl.currentTime > 0) {
            return
          }
          if (videoEl.readyState < HTMLMediaElement.HAVE_METADATA) {
            scheduleInitialStallRecovery()
            return
          }

          const duration = Number.isFinite(videoEl.duration) ? videoEl.duration : 0
          if (duration <= 0) return

          hasRecoveredInitialStall = true
          videoEl.currentTime = Math.min(0.1, duration)
        }, 1000)
      }

      const handleTimeUpdate = () => {
        if (videoEl.currentTime > 0) {
          clearStallCheck()
        }
      }

      const handlePause = () => {
        clearStallCheck()
      }

      const handleVideoError = () => {
        if (disposed) return
        clearStallCheck()
        if (!hasTriedHlsFallback && activeUrl.includes('/files/content')) {
          hasTriedHlsFallback = true
          if (contentFallbackInProgress) return
          contentFallbackInProgress = true
          // A native video element exposes no HTTP status for `error`. Probe
          // one byte before starting FFmpeg so authentication/file failures do
          // not get misreported as a transcoding failure.
          const probeHeaders: Record<string, string> = { Range: 'bytes=0-0' }
          const probeSessionId = api.getSessionId()
          if (probeSessionId) probeHeaders['X-Session-ID'] = probeSessionId
          void fetch(activeUrl, {
            credentials: 'include',
            headers: probeHeaders,
            signal: probeController.signal,
          }).then(async (response) => {
            await response.body?.cancel()
            if (disposed) return
            if (response.status === 401 || response.status === 403) {
              setErrorState({ baseUrl: activeBaseUrl, message: '登录状态已失效，请重新登录' })
              return
            }
            if (response.status === 404) {
              setErrorState({ baseUrl: activeBaseUrl, message: '视频文件不存在或已被删除' })
              return
            }
            if (response.status >= 500) {
              setErrorState({ baseUrl: activeBaseUrl, message: '文件服务暂时不可用，请重试' })
              return
            }
            if (!response.ok) {
              setErrorState({ baseUrl: activeBaseUrl, message: '无法读取视频文件（HTTP ' + response.status + '）' })
              return
            }
            stopStream()
            setErrorState(null)
            setSourceState({
              baseUrl: activeBaseUrl,
              url: activeUrl.replace('/files/content', '/files/hls'),
            })
          }).catch((err: unknown) => {
            if (disposed || (err instanceof DOMException && err.name === 'AbortError')) return
            stopStream()
            setErrorState(null)
            setSourceState({
              baseUrl: activeBaseUrl,
              url: activeUrl.replace('/files/content', '/files/hls'),
            })
          })
          return
        }
        stopStream()
        setErrorState({ baseUrl: activeBaseUrl, message: '视频转码播放失败' })
      }

      videoEl.addEventListener('webkitendfullscreen', releaseOrientationLock)
      document.addEventListener('fullscreenchange', handleFullscreenChange)
      videoEl.addEventListener('play', scheduleInitialStallRecovery)
      videoEl.addEventListener('timeupdate', handleTimeUpdate)
      videoEl.addEventListener('pause', handlePause)
      videoEl.addEventListener('error', handleVideoError)
      art.on('error', handleVideoError)
      art.on('video:error', handleVideoError)

      art.on('destroy', () => {
        disposed = true
        destroyHlsInstances()
        videoEl.removeEventListener('webkitendfullscreen', releaseOrientationLock)
        document.removeEventListener('fullscreenchange', handleFullscreenChange)
        videoEl.removeEventListener('play', scheduleInitialStallRecovery)
        videoEl.removeEventListener('timeupdate', handleTimeUpdate)
        videoEl.removeEventListener('pause', handlePause)
        videoEl.removeEventListener('error', handleVideoError)
        clearStallCheck()
        releaseOrientationLock()
      })
    }

    artRef.current = art

    return () => {
      disposed = true
      probeController.abort()
      stopStream()
      if (stopHlsRef.current === stopStream) {
        stopHlsRef.current = () => {}
      }
      if (videoRef.current === videoEl) {
        videoRef.current = null
      }
      unlockOrientationRef.current?.()
      unlockOrientationRef.current = null
      if (artRef.current === art) {
        art.destroy(false)
        artRef.current = null
      }
    }
  }, [currentUrl, retryNonce, requestNativeFullscreen, url])

  useEffect(() => {
    const handlePageHide = () => stopHlsRef.current()
    window.addEventListener('pagehide', handlePageHide)
    return () => {
      window.removeEventListener('pagehide', handlePageHide)
    }
  }, [])

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
        <div className="flex items-center gap-1">
          {/iPad|iPhone|iPod/.test(navigator.userAgent) && (
            <button
              onClick={() => requestNativeFullscreen(videoRef.current)}
              className="p-2 text-gray-400 hover:text-white transition-colors"
              title="原生全屏"
            >
              <Maximize className="w-5 h-5" />
            </button>
          )}
          <button onClick={onClose} className="p-2 text-gray-400 hover:text-white transition-colors">
            <X className="w-6 h-6" />
          </button>
        </div>
      </div>

      {/* Player */}
      <div className="flex-1 flex items-center justify-center px-4 pb-4" onClick={(e) => e.stopPropagation()}>
        <div className="w-full max-w-6xl h-[60vh] sm:h-[75vh] touch-manipulation">
          <div ref={containerRef} className="w-full h-full" />
          {error && (
            <div className="mt-3 flex items-center justify-center gap-3 text-sm text-red-300">
              <AlertCircle className="w-4 h-4" />
              <span>{error}</span>
              <button
                type="button"
                onClick={(event) => {
                  event.stopPropagation()
                  setErrorState(null)
                  setSourceState({ baseUrl: url, url })
                  setRetryNonce((value) => value + 1)
                }}
                className="inline-flex items-center gap-1 rounded border border-red-400/40 px-2 py-1 text-red-200 hover:bg-red-400/10"
              >
                <RefreshCw className="w-3.5 h-3.5" />
                重试
              </button>
            </div>
          )}
        </div>
      </div>
    </div>
  )
}
