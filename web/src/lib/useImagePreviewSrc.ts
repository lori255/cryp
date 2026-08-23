import { useCallback, useEffect, useRef, useState } from 'react'

const heifExtRe = /\.(heic|heif)$/i
const defaultRootMargin = '200px'

export type ImagePreviewStatus = 'idle' | 'loading' | 'ready' | 'unavailable'

export interface ImagePreviewOptions {
  /** Disable all preview work for entries that cannot render a preview. */
  enabled?: boolean
  /** Wait until the observed preview container is close to the viewport. */
  lazy?: boolean
  /** How far outside the viewport work may be prefetched. */
  rootMargin?: string
}

export interface ImagePreviewResult {
  src: string
  status: ImagePreviewStatus
  onPreviewError: () => void
  previewRef: (node: HTMLElement | null) => void
}

type DecodedImage = {
  get_width: () => number
  get_height: () => number
  display: (imageData: ImageData, cb: (displayData?: ImageData) => void) => void
}

type LibheifModule = {
  HeifDecoder: new () => {
    decode: (data: Uint8Array) => DecodedImage[]
  }
}

function abortError(): DOMException {
  return new DOMException('Aborted', 'AbortError')
}

function throwIfAborted(signal?: AbortSignal): void {
  if (signal?.aborted) {
    throw abortError()
  }
}

async function decodeHeifToJpegBlob(inputBlob: Blob, signal?: AbortSignal): Promise<Blob> {
  throwIfAborted(signal)
  const mod = await import('libheif-js/wasm-bundle')
  throwIfAborted(signal)
  const libheif = (mod.default ?? mod) as unknown as LibheifModule

  const decoder = new libheif.HeifDecoder()
  const bytes = new Uint8Array(await inputBlob.arrayBuffer())
  throwIfAborted(signal)
  const images = decoder.decode(bytes)
  if (!images || images.length === 0) {
    throw new Error('no decodable HEIF image found')
  }

  const first = images[0]
  const width = first.get_width()
  const height = first.get_height()
  const canvas = document.createElement('canvas')
  canvas.width = width
  canvas.height = height

  const ctx = canvas.getContext('2d')
  if (!ctx) {
    throw new Error('canvas context unavailable')
  }

  const imageData = ctx.createImageData(width, height)
  await new Promise<void>((resolve, reject) => {
    let settled = false
    const finish = (callback: () => void) => {
      if (settled) return
      settled = true
      callback()
    }

    if (signal?.aborted) {
      finish(() => reject(abortError()))
      return
    }

    const onAbort = () => finish(() => reject(abortError()))
    signal?.addEventListener('abort', onAbort, { once: true })

    first.display(imageData, (displayData) => {
      signal?.removeEventListener('abort', onAbort)
      finish(() => {
        if (!displayData) {
          reject(new Error('HEIF decode failed'))
          return
        }
        ctx.putImageData(displayData, 0, 0)
        resolve()
      })
    })
  })

  throwIfAborted(signal)
  return new Promise<Blob>((resolve, reject) => {
    canvas.toBlob((blob) => {
      if (!blob) {
        reject(new Error('JPEG encode failed'))
        return
      }
      resolve(blob)
    }, 'image/jpeg', 0.9)
  })
}

function isApplePlatform(): boolean {
  if (typeof navigator === 'undefined') {
    return false
  }

  const ua = navigator.userAgent
  const isIOS = /iPhone|iPad|iPod/i.test(ua) || (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1)
  const isMac = /Macintosh|Mac OS X/i.test(ua) || navigator.platform === 'MacIntel'
  return isIOS || isMac
}

function wait(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) {
      reject(abortError())
      return
    }

    let settled = false
    const timer = window.setTimeout(() => {
      settled = true
      signal?.removeEventListener('abort', onAbort)
      resolve()
    }, ms)
    const onAbort = () => {
      if (settled) return
      settled = true
      window.clearTimeout(timer)
      reject(abortError())
    }
    signal?.addEventListener('abort', onAbort, { once: true })
  })
}

async function fetchThumbnailBlob(thumbnailUrl: string, signal: AbortSignal): Promise<Blob> {
  const attempts = 12
  for (let i = 0; i < attempts; i++) {
    throwIfAborted(signal)
    const res = await fetch(thumbnailUrl, { credentials: 'include', signal })
    if (res.ok) {
      const blob = await res.blob()
      throwIfAborted(signal)
      return blob
    }
    if (res.status !== 404 || i === attempts - 1) {
      throw new Error(`failed to fetch thumbnail: ${res.status}`)
    }
    await wait(800, signal)
  }
  throw new Error('thumbnail unavailable')
}

function revokeObjectUrl(objectUrlRef: { current: string | null }): void {
  if (!objectUrlRef.current) return
  URL.revokeObjectURL(objectUrlRef.current)
  objectUrlRef.current = null
}

export function useImagePreviewSrc(
  fileName: string,
  contentUrl: string,
  thumbnailUrl?: string,
  options: ImagePreviewOptions = {},
): ImagePreviewResult {
  const enabled = options.enabled ?? true
  const lazy = options.lazy ?? false
  const rootMargin = options.rootMargin ?? defaultRootMargin
  const previewKey = `${fileName}\u0000${contentUrl}\u0000${thumbnailUrl ?? ''}`
  const [previewNode, setPreviewNode] = useState<HTMLElement | null>(null)
  const [visibleNode, setVisibleNode] = useState<HTMLElement | null>(null)
  const [src, setSrc] = useState('')
  const [status, setStatus] = useState<ImagePreviewStatus>('idle')
  const [conversionKey, setConversionKey] = useState<string | null>(null)
  const objectUrlRef = useRef<string | null>(null)
  const resolvedKeyRef = useRef<string | null>(null)

  const previewRef = useCallback((node: HTMLElement | null) => {
    setPreviewNode(node)
    if (!node) {
      setVisibleNode(null)
    }
  }, [])

  useEffect(() => {
    if (!enabled) {
      setVisibleNode(null)
      return
    }
    if (!lazy) {
      setVisibleNode(previewNode)
      return
    }
    if (!previewNode) {
      setVisibleNode(null)
      return
    }
    if (typeof IntersectionObserver === 'undefined') {
      setVisibleNode(previewNode)
      return
    }

    let active = true
    setVisibleNode(null)
    const observer = new IntersectionObserver((entries) => {
      if (!active) return
      const entry = entries[0]
      if (!entry) return
      setVisibleNode(entry.isIntersecting || entry.intersectionRatio > 0 ? previewNode : null)
    }, { rootMargin, threshold: 0.01 })
    observer.observe(previewNode)
    return () => {
      active = false
      observer.disconnect()
    }
  }, [enabled, lazy, previewNode, rootMargin])

  const shouldLoad = enabled && (!lazy || (previewNode !== null && visibleNode === previewNode))
  const forceConvert = conversionKey === previewKey

  useEffect(() => {
    let cancelled = false

    const cleanup = () => {
      cancelled = true
      revokeObjectUrl(objectUrlRef)
      if (resolvedKeyRef.current === previewKey) {
        resolvedKeyRef.current = null
      }
    }

    if (!shouldLoad) {
      revokeObjectUrl(objectUrlRef)
      setSrc('')
      setStatus('idle')
      return cleanup
    }

    resolvedKeyRef.current = previewKey
    const needsFallbackPath = heifExtRe.test(fileName) && !isApplePlatform()
    setSrc(needsFallbackPath ? '' : contentUrl)
    setStatus(needsFallbackPath ? 'loading' : 'ready')

    if (!heifExtRe.test(fileName)) {
      revokeObjectUrl(objectUrlRef)
      return cleanup
    }

    if (isApplePlatform()) {
      return cleanup
    }

    if (thumbnailUrl && !forceConvert) {
      const controller = new AbortController()
      const run = async () => {
        try {
          const outputBlob = await fetchThumbnailBlob(thumbnailUrl, controller.signal)
          throwIfAborted(controller.signal)
          const objectUrl = URL.createObjectURL(outputBlob)

          if (cancelled) {
            URL.revokeObjectURL(objectUrl)
            return
          }

          revokeObjectUrl(objectUrlRef)
          objectUrlRef.current = objectUrl
          setSrc(objectUrl)
          setStatus('ready')
        } catch (err) {
          if (!cancelled) {
            console.warn('HEIF thumbnail failed', err)
            setSrc('')
            setStatus('unavailable')
          }
        }
      }

      void run()
      return () => {
        cleanup()
        controller.abort()
      }
    }

    if (!forceConvert) {
      const probe = new Image()
      probe.onload = () => {
        if (!cancelled) {
          setSrc(contentUrl)
          setStatus('ready')
        }
      }
      probe.onerror = () => {
        if (!cancelled) {
          setConversionKey(previewKey)
        }
      }
      probe.src = contentUrl
      return () => {
        probe.onload = null
        probe.onerror = null
        probe.src = ''
        cleanup()
      }
    }

    const controller = new AbortController()
    const run = async () => {
      try {
        const signal = controller.signal
        const res = await fetch(contentUrl, { credentials: 'include', signal })
        if (!res.ok) {
          throw new Error(`failed to fetch image: ${res.status}`)
        }
        throwIfAborted(signal)
        const inputBlob = await res.blob()
        throwIfAborted(signal)
        const outputBlob = await decodeHeifToJpegBlob(inputBlob, signal)
        throwIfAborted(signal)
        const objectUrl = URL.createObjectURL(outputBlob)

        if (cancelled) {
          URL.revokeObjectURL(objectUrl)
          return
        }

        revokeObjectUrl(objectUrlRef)
        objectUrlRef.current = objectUrl
        setSrc(objectUrl)
        setStatus('ready')
      } catch (err) {
        if (!cancelled) {
          console.warn('HEIF conversion failed', err)
          setSrc('')
          setStatus('unavailable')
        }
      }
    }

    void run()
    return () => {
      cleanup()
      controller.abort()
    }
  }, [contentUrl, fileName, forceConvert, previewKey, shouldLoad, thumbnailUrl])

  const onPreviewError = useCallback(() => {
    if (!enabled || !shouldLoad || isApplePlatform()) return
    setStatus('loading')
    setConversionKey(previewKey)
  }, [enabled, previewKey, shouldLoad])

  const isCurrentPreview = shouldLoad && resolvedKeyRef.current === previewKey
  return {
    src: isCurrentPreview ? src : '',
    status: isCurrentPreview ? status : 'idle',
    onPreviewError,
    previewRef,
  }
}
