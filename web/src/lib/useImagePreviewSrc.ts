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

export function useImagePreviewSrc(fileName: string, contentUrl: string): {
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

    const run = async () => {
      try {
        setStatus('loading')
        const res = await fetch(contentUrl, { credentials: 'include' })
        if (!res.ok) {
          throw new Error(`failed to fetch image: ${res.status}`)
        }

        const inputBlob = await res.blob()
        const outputBlob = await decodeHeifToJpegBlob(inputBlob)
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
        console.warn('HEIF conversion failed', err)
        setSrc('')
        setStatus('unavailable')
      }
    }

    void run()

    return () => {
      cancelled = true
      if (objectUrlRef.current) {
        URL.revokeObjectURL(objectUrlRef.current)
        objectUrlRef.current = null
      }
    }
  }, [fileName, contentUrl, forceConvert])

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
