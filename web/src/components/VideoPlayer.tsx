import { useCallback, useEffect, useId, useRef, useState } from 'react'
import Artplayer from 'artplayer'
import Hls from 'hls.js'
import { AlertCircle, Maximize, RefreshCw, X } from 'lucide-react'
import { api } from '../lib/api'
import {
  getNativeVideo,
  isIOSDevice,
  lockLandscapeOrientation,
  requestVideoFullscreen,
} from '../lib/videoFullscreen'
import { durationFromUrl, installStableTimeline } from '../lib/videoTimeline'

interface VideoPlayerProps {
  url: string
  title: string
  onClose: () => void
}

const HLS_RESUME_THRESHOLD_MS = 90_000

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
  const fullscreenVideoRef = useRef<HTMLVideoElement | null>(null)
  const unlockOrientationRef = useRef<(() => void) | null>(null)
  const stopHlsRef = useRef<() => void>(() => {})
  const modalRef = useRef<HTMLDivElement>(null)
  const closeButtonRef = useRef<HTMLButtonElement>(null)
  const hiddenAtRef = useRef<number | null>(null)
  const wasPlayingBeforeHideRef = useRef(false)
  const resumePositionRef = useRef<number | null>(null)
  const titleId = useId()
  const errorId = useId()
  const isIOS = isIOSDevice()

  const showPlayerError = useCallback((message: string) => {
    setErrorState({ baseUrl: url, message })
  }, [url])

  const requestNativeFullscreen = useCallback((video: HTMLVideoElement | null) => {
    if (!video) {
      showPlayerError('视频尚未准备好，暂时无法进入全屏')
      return
    }

    unlockOrientationRef.current?.()
    unlockOrientationRef.current = null
    fullscreenVideoRef.current = video

    // Do not await anything before this call. WebKit requires the native
    // fullscreen method to run inside the original button's user gesture.
    const request = requestVideoFullscreen(video)
    if (!request.method) {
      fullscreenVideoRef.current = null
      showPlayerError('当前浏览器不支持视频全屏，请使用 Safari 或更新系统')
      return
    }

    if (request.promise) {
      void request.promise.catch(() => {
        if (fullscreenVideoRef.current !== video) return
        fullscreenVideoRef.current = null
        unlockOrientationRef.current?.()
        unlockOrientationRef.current = null
        showPlayerError('浏览器拒绝了全屏请求，请再次点击全屏按钮')
      })
    }
  }, [showPlayerError])

  useEffect(() => {
    if (!containerRef.current) return

    const activeUrl = currentUrl
    const activeBaseUrl = url
    let disposed = false
    let streamStopped = false
    let activeHlsUrl = activeUrl
    let stableDuration = durationFromUrl(activeUrl)
    let refreshTimeline = () => {}
    let terminalErrorHandled = false
    const rememberStableDuration = (candidate: string) => {
      const duration = durationFromUrl(candidate)
      if (duration <= 0) return
      stableDuration = duration
      refreshTimeline()
    }
    const probeController = new AbortController()
    const stopStream = () => {
      if (streamStopped) return
      streamStopped = true
      api.stopHls(activeHlsUrl)
    }
    stopHlsRef.current = stopStream

    const hlsInstances: Hls[] = []
    const destroyHlsInstances = () => {
      while (hlsInstances.length > 0) {
        hlsInstances.pop()?.destroy()
      }
    }

    const art = new Artplayer({
      container: containerRef.current,
      url: activeUrl,
      // Artplayer validates `type` at construction time. The content endpoint
      // is intentionally used for native-compatible media, so it still needs
      // an explicit non-HLS type instead of `undefined`.
      type: activeUrl.includes('/files/hls') || activeUrl.includes('.m3u8') ? 'm3u8' : 'mp4',
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
      moreVideoAttr: {
        playsInline: true,
        preload: 'metadata',
      },
      controls: isIOS ? [{
        name: 'ios-native-fullscreen',
        position: 'right',
        index: 20,
        tooltip: '原生全屏',
        html: '<div class="art-icon"><svg viewBox="0 0 24 24" width="22" height="22" aria-hidden="true"><path fill="currentColor" d="M7 3H3v4h2V5h2V3zm14 0h-4v2h2v2h2V3zM5 17H3v4h4v-2H5v-2zm16 0h-2v2h-2v2h4v-4z"/></svg></div>',
        click: () => requestNativeFullscreen(videoRef.current),
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
              // Native video elements cannot receive our X-Session-ID header,
              // so hls.js must carry the session cookie on cross-origin media
              // requests. Fetch also gets the header when localStorage auth is
              // the available credential.
              xhrSetup: (xhr) => {
                xhr.withCredentials = true
              },
              fetchSetup: (context, initParams) => {
                initParams.credentials = 'include'
                const sessionId = api.getSessionId()
                if (sessionId) {
                  const headers = new Headers(initParams.headers)
                  headers.set('X-Session-ID', sessionId)
                  initParams.headers = headers
                }
                return new Request(context.url, initParams)
              },
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
                    rememberStableDuration(candidate)
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
              if (disposed || !data.fatal || terminalErrorHandled) return

              // A single media recovery handles transient decoder state. Do
              // not retry fatal network errors indefinitely: each retry can
              // otherwise create another slow backend HLS startup.
              if (data.type === 'mediaError' && mediaRecoveryCount < 1) {
                mediaRecoveryCount++
                hls.recoverMediaError()
                return
              }

              terminalErrorHandled = true
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
              } else if (status === 409) {
                message = '缺少或无法读取媒体索引，请先重建文件索引'
              } else if (status === 404) {
                message = '视频或转码分片不存在'
              } else if (status === 429) {
                message = '转码资源繁忙，请稍后重试'
              } else if (typeof status === 'number' && status >= 500) {
                message = '转码服务暂时不可用，请重试'
              } else if (data.type === 'networkError') {
                message = '网络连接失败，请检查网络后重试'
              } else if (data.type === 'mediaError') {
                message = '浏览器无法解码该视频，请尝试转码播放'
              }
              setErrorState({ baseUrl: activeBaseUrl, message })
            })
            return
          }

          if (video.canPlayType('application/vnd.apple.mpegurl')) {
            video.src = sourceUrl
            return
          }
          terminalErrorHandled = true
          setErrorState({ baseUrl: activeBaseUrl, message: '当前浏览器不支持 HLS 视频播放' })
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
      const timeline = installStableTimeline(art, videoEl, () => stableDuration)
      refreshTimeline = timeline.refresh
      const refreshStableTimeline = () => {
        if (videoEl.currentSrc) rememberStableDuration(videoEl.currentSrc)
        timeline.refresh()
      }

      let pendingResumeTime = resumePositionRef.current
      resumePositionRef.current = null
      const restorePlaybackPosition = () => {
        if (pendingResumeTime === null || !Number.isFinite(pendingResumeTime)) return
        const duration = Number.isFinite(videoEl.duration) ? videoEl.duration : pendingResumeTime
        videoEl.currentTime = Math.min(Math.max(0, pendingResumeTime), Math.max(0, duration - 0.05))
        pendingResumeTime = null
      }
      if (pendingResumeTime !== null) {
        videoEl.addEventListener('loadedmetadata', restorePlaybackPosition)
      }

      const releaseOrientationLock = () => {
        if (fullscreenVideoRef.current !== videoEl) return
        unlockOrientationRef.current?.()
        unlockOrientationRef.current = null
        fullscreenVideoRef.current = null
      }

      const lockOrientationForFullscreen = () => {
        if (disposed || fullscreenVideoRef.current !== videoEl) return

        void lockLandscapeOrientation().then((unlockOrientation) => {
          if (disposed || fullscreenVideoRef.current !== videoEl) {
            unlockOrientation?.()
            return
          }
          unlockOrientationRef.current?.()
          unlockOrientationRef.current = unlockOrientation
        })
      }

      const handleNativeFullscreenStart = () => {
        fullscreenVideoRef.current = videoEl
        lockOrientationForFullscreen()
      }

      const handlePresentationModeChange = () => {
        const mode = getNativeVideo(videoEl).webkitPresentationMode
        if (mode === 'fullscreen') {
          handleNativeFullscreenStart()
        } else if (mode === 'inline') {
          releaseOrientationLock()
        }
      }

      const handleFullscreenChange = () => {
        if (document.fullscreenElement === videoEl) {
          handleNativeFullscreenStart()
        } else if (!document.fullscreenElement) {
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
        timeline.refresh()
      }

      const handlePause = () => {
        clearStallCheck()
      }

      const handleVideoError = () => {
        if (disposed || terminalErrorHandled || contentFallbackInProgress) return
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
            terminalErrorHandled = true
            stopStream()
            setErrorState(null)
            setSourceState({
              baseUrl: activeBaseUrl,
              url: activeUrl.replace('/files/content', '/files/hls'),
            })
          }).catch((err: unknown) => {
            if (disposed || (err instanceof DOMException && err.name === 'AbortError')) return
            stopStream()
            setErrorState({ baseUrl: activeBaseUrl, message: '网络连接失败，请检查网络后重试' })
          })
          return
        }
        terminalErrorHandled = true
        stopStream()
        setErrorState({ baseUrl: activeBaseUrl, message: '视频转码播放失败' })
      }

      videoEl.addEventListener('webkitbeginfullscreen', handleNativeFullscreenStart)
      videoEl.addEventListener('webkitendfullscreen', releaseOrientationLock)
      videoEl.addEventListener('webkitpresentationmodechanged', handlePresentationModeChange)
      document.addEventListener('fullscreenchange', handleFullscreenChange)
      videoEl.addEventListener('play', scheduleInitialStallRecovery)
      videoEl.addEventListener('loadedmetadata', refreshStableTimeline)
      videoEl.addEventListener('durationchange', refreshStableTimeline)
      videoEl.addEventListener('progress', refreshStableTimeline)
      videoEl.addEventListener('timeupdate', handleTimeUpdate)
      videoEl.addEventListener('pause', handlePause)
      videoEl.addEventListener('error', handleVideoError)
      art.on('error', handleVideoError)
      art.on('video:error', handleVideoError)

      art.on('destroy', () => {
        disposed = true
        destroyHlsInstances()
        videoEl.removeEventListener('webkitbeginfullscreen', handleNativeFullscreenStart)
        videoEl.removeEventListener('webkitendfullscreen', releaseOrientationLock)
        videoEl.removeEventListener('webkitpresentationmodechanged', handlePresentationModeChange)
        document.removeEventListener('fullscreenchange', handleFullscreenChange)
        videoEl.removeEventListener('loadedmetadata', restorePlaybackPosition)
        videoEl.removeEventListener('play', scheduleInitialStallRecovery)
        videoEl.removeEventListener('loadedmetadata', refreshStableTimeline)
        videoEl.removeEventListener('durationchange', refreshStableTimeline)
        videoEl.removeEventListener('progress', refreshStableTimeline)
        videoEl.removeEventListener('timeupdate', handleTimeUpdate)
        videoEl.removeEventListener('pause', handlePause)
        videoEl.removeEventListener('error', handleVideoError)
        clearStallCheck()
        timeline.destroy()
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
      if (fullscreenVideoRef.current === videoEl) {
        unlockOrientationRef.current?.()
        unlockOrientationRef.current = null
        fullscreenVideoRef.current = null
      }
      if (artRef.current === art) {
        art.destroy(false)
        artRef.current = null
      }
    }
  }, [currentUrl, isIOS, retryNonce, requestNativeFullscreen, url])

  useEffect(() => {
    const markHidden = () => {
      if (document.visibilityState !== 'hidden') return
      hiddenAtRef.current = Date.now()
      wasPlayingBeforeHideRef.current = Boolean(videoRef.current && !videoRef.current.paused)
    }

    const resumeIfStale = () => {
      const hiddenAt = hiddenAtRef.current
      hiddenAtRef.current = null
      if (!hiddenAt || !wasPlayingBeforeHideRef.current) return
      wasPlayingBeforeHideRef.current = false

      if (Date.now() - hiddenAt < HLS_RESUME_THRESHOLD_MS) return
      resumePositionRef.current = videoRef.current?.currentTime ?? null
      setRetryNonce((value) => value + 1)
    }

    const handleVisibilityChange = () => {
      if (document.visibilityState === 'hidden') {
        markHidden()
      } else {
        resumeIfStale()
      }
    }

    const handlePageHide = (event: PageTransitionEvent) => {
      if (event.persisted) {
        markHidden()
        return
      }
      // A non-persisted pagehide is a real unload/navigation. A bfcache
      // transition keeps the stream alive so pageshow can recover it.
      stopHlsRef.current()
    }

    const handlePageShow = () => resumeIfStale()
    document.addEventListener('visibilitychange', handleVisibilityChange)
    window.addEventListener('pagehide', handlePageHide)
    window.addEventListener('pageshow', handlePageShow)
    return () => {
      document.removeEventListener('visibilitychange', handleVisibilityChange)
      window.removeEventListener('pagehide', handlePageHide)
      window.removeEventListener('pageshow', handlePageShow)
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
    const modal = modalRef.current
    const previousActiveElement = document.activeElement as HTMLElement | null
    closeButtonRef.current?.focus()

    const handleKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        event.preventDefault()
        onClose()
        return
      }
      if (event.key !== 'Tab' || !modal) return

      const focusable = Array.from(modal.querySelectorAll<HTMLElement>(
        'button:not([disabled]), [href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
      )).filter((element) => element.offsetParent !== null)
      if (focusable.length === 0) {
        event.preventDefault()
        modal.focus()
        return
      }

      const first = focusable[0]
      const last = focusable[focusable.length - 1]
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault()
        last.focus()
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault()
        first.focus()
      }
    }

    modal?.addEventListener('keydown', handleKeyDown)
    return () => {
      modal?.removeEventListener('keydown', handleKeyDown)
      if (previousActiveElement?.isConnected) previousActiveElement.focus()
    }
  }, [onClose])

  return (
    <div
      ref={modalRef}
      role="dialog"
      aria-modal="true"
      aria-labelledby={titleId}
      aria-describedby={error ? errorId : undefined}
      tabIndex={-1}
      className="player-modal fixed inset-0 z-50 flex flex-col bg-black/90 outline-none"
      onClick={onClose}
    >
      {/* Title bar */}
      <div className="player-modal__header flex items-center justify-between flex-shrink-0" onClick={(e) => e.stopPropagation()}>
        <h3 id={titleId} className="text-white font-medium truncate">{title}</h3>
        <div className="flex items-center gap-1">
          {isIOS && (
            <button
              type="button"
              onClick={() => requestNativeFullscreen(videoRef.current)}
              className="player-icon-button p-2 text-gray-400 transition-colors"
              title="原生全屏"
              aria-label="进入原生全屏"
            >
              <Maximize className="w-5 h-5" />
            </button>
          )}
          <button
            ref={closeButtonRef}
            type="button"
            onClick={onClose}
            className="player-icon-button p-2 text-gray-400 transition-colors"
            aria-label="关闭播放器"
            title="关闭播放器"
          >
            <X className="w-6 h-6" />
          </button>
        </div>
      </div>

      {/* Player */}
      <div className="player-modal__content flex-1 flex items-center justify-center" onClick={(e) => e.stopPropagation()}>
        <div className="player-stage w-full max-w-6xl touch-manipulation">
          <div ref={containerRef} className="w-full h-full" />
          {error && (
            <div id={errorId} role="alert" aria-live="assertive" className="player-error mt-3 flex items-center justify-center gap-3 text-sm text-red-300">
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
                className="inline-flex items-center gap-1 rounded border border-red-400/40 px-2 py-1 text-red-200 hover:bg-red-400/10 focus-visible:outline focus-visible:outline-2 focus-visible:outline-blue-400"
                aria-label="重试播放"
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
