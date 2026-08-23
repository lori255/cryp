export type PresentationMode = 'inline' | 'fullscreen' | 'picture-in-picture'

export type NativeVideoElement = HTMLVideoElement & {
  webkitSupportsFullscreen?: boolean
  webkitEnterFullscreen?: () => void
  webkitSetPresentationMode?: (mode: PresentationMode) => void
  webkitPresentationMode?: PresentationMode
}

export type FullscreenMethod = 'webkit-enter' | 'webkit-presentation' | 'standard'

export interface FullscreenRequest {
  method: FullscreenMethod | null
  promise?: Promise<void>
}

/**
 * iPadOS reports a desktop (Macintosh) user agent when “Request Desktop
 * Website” is enabled. Touch capability is the reliable discriminator in
 * that case; the explicit iPhone/iPad check covers older WebKit versions.
 */
export function isIOSDevice(): boolean {
  if (typeof navigator === 'undefined') return false

  const userAgent = navigator.userAgent
  if (/iPad|iPhone|iPod/.test(userAgent)) return true

  return navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1
}

export function getNativeVideo(video: HTMLVideoElement): NativeVideoElement {
  return video as NativeVideoElement
}

/**
 * Return the best available method without invoking it. Keeping capability
 * detection in one place prevents the control visibility check from drifting
 * away from the click handler's actual fallback order.
 */
export function getFullscreenMethod(video: HTMLVideoElement | null): FullscreenMethod | null {
  if (!video) return null

  const nativeVideo = getNativeVideo(video)
  if (
    nativeVideo.webkitSupportsFullscreen !== false
    && typeof nativeVideo.webkitEnterFullscreen === 'function'
  ) {
    return 'webkit-enter'
  }
  if (typeof nativeVideo.webkitSetPresentationMode === 'function') {
    return 'webkit-presentation'
  }
  if (typeof video.requestFullscreen === 'function') return 'standard'
  return null
}

/**
 * Invoke fullscreen synchronously so WebKit still sees the original click's
 * transient user activation. Callers can await the returned promise only
 * after this function has returned.
 */
export function requestVideoFullscreen(video: HTMLVideoElement | null): FullscreenRequest {
  if (!video) return { method: null }

  const nativeVideo = getNativeVideo(video)
  if (
    nativeVideo.webkitSupportsFullscreen !== false
    && typeof nativeVideo.webkitEnterFullscreen === 'function'
  ) {
    try {
      nativeVideo.webkitEnterFullscreen()
      return { method: 'webkit-enter' }
    } catch {
      // Try the less-specific WebKit presentation API below.
    }
  }

  if (typeof nativeVideo.webkitSetPresentationMode === 'function') {
    try {
      nativeVideo.webkitSetPresentationMode('fullscreen')
      return { method: 'webkit-presentation' }
    } catch {
      // Fall through to the standard API when WebKit rejects the mode.
    }
  }

  if (typeof video.requestFullscreen === 'function') {
    try {
      return { method: 'standard', promise: video.requestFullscreen() }
    } catch {
      // Report an unsupported/denied request to the caller.
    }
  }

  return { method: null }
}

/**
 * Orientation locking is deliberately separate from the fullscreen request:
 * iOS only allows it after fullscreen has started, and older WebViews may not
 * expose screen.orientation at all.
 */
export async function lockLandscapeOrientation(): Promise<(() => void) | null> {
  if (typeof screen === 'undefined' || !screen.orientation) return null

  const orientation = screen.orientation as ScreenOrientation & {
    lock?: (orientation: 'landscape') => Promise<void>
    unlock?: () => void
  }
  if (typeof orientation.lock !== 'function') return null

  try {
    await orientation.lock('landscape')
    return () => orientation.unlock?.()
  } catch {
    // Fullscreen remains useful even when the platform refuses orientation
    // locking (for example, in an embedded WebView or portrait-only device).
    return null
  }
}
