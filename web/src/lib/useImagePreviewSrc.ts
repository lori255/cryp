import { useEffect, useRef, useState } from 'react'

const heifExtRe = /\.(heic|heif)$/i

type LibheifModule = {
  HeifDecoder: new () => {
    decode: (data: Uint8Array) => Array<{
      get_width: () => number
      get_height: () => number
      display: (imageData: ImageData, cb: (displayData?: ImageData) => void) => void
    }>
  }
}

async function decodeHeifToJpegBlob(inputBlob: Blob): Promise<Blob> {
  const mod = await import('libheif-js/wasm-bundle')
  const libheif = (mod.default ?? mod) as unknown as LibheifModule

  const decoder = new libheif.HeifDecoder()
  const bytes = new Uint8Array(await inputBlob.arrayBuffer())
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
    first.display(imageData, (displayData) => {
      if (!displayData) {
        reject(new Error('HEIF decode failed'))
        return
      }
      ctx.putImageData(displayData, 0, 0)
      resolve()
    })
  })

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
      reject(new DOMException('Aborted', 'AbortError'))
      return
    }
    const timer = window.setTimeout(resolve, ms)
    signal?.addEventListener('abort', () => {
      window.clearTimeout(timer)
      reject(new DOMException('Aborted', 'AbortError'))
    }, { once: true })
  })
}

async function fetchThumbnailBlob(thumbnailUrl: string, shouldCancel: () => boolean, signal?: AbortSignal): Promise<Blob> {
  const attempts = 12
  for (let i = 0; i < attempts; i++) {
    const res = await fetch(thumbnailUrl, { credentials: 'include', signal })
    if (res.ok) {
      return res.blob()
    }
    if (res.status !== 404 || i === attempts - 1 || shouldCancel()) {
      throw new Error(`failed to fetch thumbnail: ${res.status}`)
    }
    await wait(800, signal)
  }
  throw new Error('thumbnail unavailable')
}

export function useImagePreviewSrc(fileName: string, contentUrl: string, thumbnailUrl?: string): {
  src: string
  status: 'loading' | 'ready' | 'unavailable'
  onPreviewError: () => void
} {
  const [src, setSrc] = useState(contentUrl)
  const [status, setStatus] = useState<'loading' | 'ready' | 'unavailable'>('ready')
  const [forceConvert, setForceConvert] = useState(false)
  const objectUrlRef = useRef<string | null>(null)

  useEffect(() => {
    const needsFallbackPath = heifExtRe.test(fileName) && !isApplePlatform()
    setSrc(needsFallbackPath ? '' : contentUrl)
    setStatus(needsFallbackPath ? 'loading' : 'ready')
    setForceConvert(false)

    return
  }, [fileName, contentUrl])

  useEffect(() => {
    let cancelled = false
    if (!heifExtRe.test(fileName)) {
      if (objectUrlRef.current) {
        URL.revokeObjectURL(objectUrlRef.current)
        objectUrlRef.current = null
      }
      setStatus('ready')
      return () => {
        cancelled = true
      }
    }

    if (isApplePlatform()) {
      setSrc(contentUrl)
      setStatus('ready')
      return () => {
        cancelled = true
      }
    }

    if (thumbnailUrl) {
      const controller = new AbortController()
      const run = async () => {
        try {
          setStatus('loading')
          setSrc('')
          const outputBlob = await fetchThumbnailBlob(thumbnailUrl, () => cancelled, controller.signal)
          const objectUrl = URL.createObjectURL(outputBlob)

          if (cancelled) {
            URL.revokeObjectURL(objectUrl)
            return
          }

          if (objectUrlRef.current) {
            URL.revokeObjectURL(objectUrlRef.current)
          }
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
        cancelled = true
        controller.abort()
        if (objectUrlRef.current) {
          URL.revokeObjectURL(objectUrlRef.current)
          objectUrlRef.current = null
        }
      }
    }

    if (!forceConvert) {
      setStatus('loading')
      const probe = new Image()
      probe.onload = () => {
        if (!cancelled) {
          setSrc(contentUrl)
          setStatus('ready')
        }
      }
      probe.onerror = () => {
        if (!cancelled) {
          setForceConvert(true)
        }
      }
      probe.src = contentUrl
      return () => {
        cancelled = true
      }
    }

    const controller = new AbortController()
    const run = async () => {
      const signal = controller.signal
      try {
        setStatus('loading')
        const res = await fetch(contentUrl, { credentials: 'include', signal })
        if (!res.ok) {
          throw new Error(`failed to fetch image: ${res.status}`)
        }
        if (cancelled) return

        const inputBlob = await res.blob()
        if (cancelled) return
        const outputBlob = await decodeHeifToJpegBlob(inputBlob)
        if (cancelled) return
        const objectUrl = URL.createObjectURL(outputBlob)

        if (cancelled) {
          URL.revokeObjectURL(objectUrl)
          return
        }

        if (objectUrlRef.current) {
          URL.revokeObjectURL(objectUrlRef.current)
        }
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
      cancelled = true
      controller.abort()
      // The WASM decode itself cannot be interrupted, but aborting fetch keeps
      // navigation away from starting or continuing large downloads.
      if (objectUrlRef.current) {
        URL.revokeObjectURL(objectUrlRef.current)
        objectUrlRef.current = null
      }
    }
  }, [fileName, contentUrl, thumbnailUrl, forceConvert])

  return {
    src,
    status,
    onPreviewError: () => {
      if (!isApplePlatform()) {
        setSrc('')
        setStatus('loading')
        setForceConvert(true)
      }
    },
  }
}
