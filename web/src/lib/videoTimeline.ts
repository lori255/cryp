import type Artplayer from 'artplayer'

export function durationFromUrl(value: string): number {
  try {
    const duration = Number(new URL(value, window.location.origin).searchParams.get('duration'))
    return Number.isFinite(duration) && duration > 0 ? duration : 0
  } catch {
    return 0
  }
}

function formatMediaTime(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return '00:00'
  const whole = Math.floor(seconds)
  const hours = Math.floor(whole / 3600)
  const minutes = Math.floor((whole % 3600) / 60)
  const remaining = whole % 60
  return hours > 0
    ? `${hours}:${String(minutes).padStart(2, '0')}:${String(remaining).padStart(2, '0')}`
    : `${String(minutes).padStart(2, '0')}:${String(remaining).padStart(2, '0')}`
}

function mediaEnd(ranges: TimeRanges): number {
  let end = 0
  for (let index = 0; index < ranges.length; index++) {
    end = Math.max(end, ranges.end(index))
  }
  return end
}

export function installStableTimeline(art: Artplayer, video: HTMLVideoElement, stableDuration: () => number) {
  const playerRoot = video.closest('.art-video-player')
  const progressRoot = playerRoot?.querySelector<HTMLElement>('.art-progress') ?? null
  const timeRoot = playerRoot?.querySelector<HTMLElement>('.art-control-time') ?? null
  if (!progressRoot) return { refresh: () => {}, destroy: () => {} }

  const control = document.createElement('div')
  control.className = 'art-control-progress'
  control.setAttribute('role', 'slider')
  control.setAttribute('aria-label', '视频播放进度')
  control.tabIndex = 0
  const inner = document.createElement('div')
  inner.className = 'art-control-progress-inner'
  const loaded = document.createElement('div')
  loaded.className = 'art-progress-loaded'
  const played = document.createElement('div')
  played.className = 'art-progress-played'
  const indicator = document.createElement('div')
  indicator.className = 'art-progress-indicator'
  indicator.style.background = 'var(--art-theme)'
  inner.append(loaded, played, indicator)
  control.append(inner)
  progressRoot.replaceChildren(control)

  const timeText = document.createElement('span')
  if (timeRoot) timeRoot.replaceChildren(timeText)

  const effectiveDuration = () => {
    const stable = stableDuration()
    if (stable > 0) return stable
    return Number.isFinite(video.duration) && video.duration > 0 ? video.duration : 0
  }
  const availableEnd = () => {
    const seekableEnd = mediaEnd(video.seekable)
    const nativeDuration = Number.isFinite(video.duration) && video.duration > 0 ? video.duration : 0
    return Math.max(seekableEnd, nativeDuration)
  }
  const refresh = () => {
    const duration = effectiveDuration()
    const current = Math.max(0, video.currentTime || 0)
    const buffered = mediaEnd(video.buffered)
    const playedRatio = duration > 0 ? Math.min(1, current / duration) : 0
    const loadedRatio = duration > 0 ? Math.min(1, buffered / duration) : 0
    played.style.width = `${playedRatio * 100}%`
    indicator.style.left = `${playedRatio * 100}%`
    loaded.style.width = `${loadedRatio * 100}%`
    control.setAttribute('aria-valuemin', '0')
    control.setAttribute('aria-valuemax', String(Math.round(duration)))
    control.setAttribute('aria-valuenow', String(Math.round(current)))
    if (timeRoot) timeText.textContent = `${formatMediaTime(current)} / ${duration > 0 ? formatMediaTime(duration) : '--:--'}`
  }
  const seekToRatio = (ratio: number) => {
    const duration = effectiveDuration()
    if (duration <= 0) return
    const requested = Math.min(1, Math.max(0, ratio)) * duration
    const available = Math.min(duration, availableEnd())
    if (stableDuration() > 0 && requested > available + 0.1) {
      // A growing HLS playlist can briefly expose no seekable range at all.
      // Keep the current position instead of snapping back to the beginning;
      // once FFmpeg publishes more segments the same target becomes seekable.
      if (available > 0.1) video.currentTime = Math.max(0, available - 0.05)
      art.notice.show = '该位置仍在转码，请稍后再试'
    } else {
      video.currentTime = requested
    }
    refresh()
  }
  const seekFromPointer = (event: PointerEvent) => {
    const rect = control.getBoundingClientRect()
    if (rect.width > 0) seekToRatio((event.clientX - rect.left) / rect.width)
  }
  let draggingPointer: number | null = null
  const handlePointerDown = (event: PointerEvent) => {
    draggingPointer = event.pointerId
    control.setPointerCapture(event.pointerId)
    seekFromPointer(event)
  }
  const handlePointerMove = (event: PointerEvent) => {
    if (draggingPointer === event.pointerId) seekFromPointer(event)
  }
  const handlePointerUp = (event: PointerEvent) => {
    if (draggingPointer !== event.pointerId) return
    seekFromPointer(event)
    draggingPointer = null
    if (control.hasPointerCapture(event.pointerId)) control.releasePointerCapture(event.pointerId)
  }
  const handleKeyDown = (event: KeyboardEvent) => {
    if (event.key !== 'ArrowLeft' && event.key !== 'ArrowRight') return
    event.preventDefault()
    event.stopPropagation()
    const duration = effectiveDuration()
    if (duration <= 0) return
    const delta = event.key === 'ArrowLeft' ? -5 : 5
    seekToRatio((video.currentTime + delta) / duration)
  }
  control.addEventListener('pointerdown', handlePointerDown)
  control.addEventListener('pointermove', handlePointerMove)
  control.addEventListener('pointerup', handlePointerUp)
  control.addEventListener('pointercancel', handlePointerUp)
  control.addEventListener('keydown', handleKeyDown)
  refresh()
  return {
    refresh,
    destroy: () => {
      control.removeEventListener('pointerdown', handlePointerDown)
      control.removeEventListener('pointermove', handlePointerMove)
      control.removeEventListener('pointerup', handlePointerUp)
      control.removeEventListener('pointercancel', handlePointerUp)
      control.removeEventListener('keydown', handleKeyDown)
    },
  }
}
