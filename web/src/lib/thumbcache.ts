const DB_NAME = 'cryp-thumbcache'
const STORE_NAME = 'thumbnails'
const DB_VERSION = 1
const MAX_CONCURRENT = 3
const THUMB_WIDTH = 320
const THUMB_HEIGHT = 180

let dbPromise: Promise<IDBDatabase> | null = null

function openDB(): Promise<IDBDatabase> {
  if (dbPromise) return dbPromise
  dbPromise = new Promise((resolve, reject) => {
    const req = indexedDB.open(DB_NAME, DB_VERSION)
    req.onupgradeneeded = () => {
      const db = req.result
      if (!db.objectStoreNames.contains(STORE_NAME)) {
        db.createObjectStore(STORE_NAME)
      }
    }
    req.onsuccess = () => resolve(req.result)
    req.onerror = () => reject(req.error)
  })
  return dbPromise
}

export async function getCachedThumb(key: string): Promise<string | null> {
  try {
    const db = await openDB()
    return new Promise((resolve) => {
      const tx = db.transaction(STORE_NAME, 'readonly')
      const req = tx.objectStore(STORE_NAME).get(key)
      req.onsuccess = () => resolve(req.result ?? null)
      req.onerror = () => resolve(null)
    })
  } catch {
    return null
  }
}

export async function setCachedThumb(key: string, dataUrl: string): Promise<void> {
  try {
    const db = await openDB()
    const tx = db.transaction(STORE_NAME, 'readwrite')
    tx.objectStore(STORE_NAME).put(dataUrl, key)
  } catch {
    // Silently fail — cache is optional
  }
}

// --- Concurrency-limited thumbnail generation queue ---

type ThumbTask = {
  src: string
  type: 'image' | 'video'
  resolve: (dataUrl: string | null) => void
}

const queue: ThumbTask[] = []
let running = 0

function processQueue() {
  while (running < MAX_CONCURRENT && queue.length > 0) {
    const task = queue.shift()!
    running++
    generateThumb(task.src, task.type).then((result) => {
      task.resolve(result)
      running--
      processQueue()
    })
  }
}

export function enqueueThumbGeneration(src: string, type: 'image' | 'video'): Promise<string | null> {
  return new Promise((resolve) => {
    queue.push({ src, type, resolve })
    processQueue()
  })
}

// Clear pending queue (e.g. when navigating away from directory)
export function clearThumbQueue() {
  queue.length = 0
}

function generateThumb(src: string, type: 'image' | 'video'): Promise<string | null> {
  if (type === 'video') return generateVideoThumb(src)
  return generateImageThumb(src)
}

function generateVideoThumb(src: string): Promise<string | null> {
  return new Promise((resolve) => {
    const video = document.createElement('video')
    video.preload = 'auto'
    video.muted = true
    video.setAttribute('playsinline', '')
    video.setAttribute('webkit-playsinline', '')

    const timeout = setTimeout(() => {
      cleanup()
      resolve(null)
    }, 15000)

    const cleanup = () => {
      clearTimeout(timeout)
      video.removeAttribute('src')
      video.load()
    }

    video.addEventListener('loadeddata', () => {
      video.currentTime = Math.min(1, video.duration || 1)
    })

    video.addEventListener('seeked', () => {
      try {
        const dataUrl = canvasCapture(video, video.videoWidth, video.videoHeight)
        resolve(dataUrl)
      } catch {
        resolve(null)
      }
      cleanup()
    })

    video.addEventListener('error', () => {
      cleanup()
      resolve(null)
    })

    video.src = src
    video.load()
  })
}

function generateImageThumb(src: string): Promise<string | null> {
  return new Promise((resolve) => {
    const img = document.createElement('img')

    const timeout = setTimeout(() => {
      resolve(null)
    }, 15000)

    img.onload = () => {
      clearTimeout(timeout)
      try {
        const dataUrl = canvasCapture(img, img.naturalWidth, img.naturalHeight)
        resolve(dataUrl)
      } catch {
        resolve(null)
      }
    }

    img.onerror = () => {
      clearTimeout(timeout)
      resolve(null)
    }

    img.src = src
  })
}

function canvasCapture(
  source: HTMLVideoElement | HTMLImageElement,
  srcWidth: number,
  srcHeight: number,
): string | null {
  if (!srcWidth || !srcHeight) return null
  // Scale down to thumbnail size, preserving aspect ratio
  const scale = Math.min(THUMB_WIDTH / srcWidth, THUMB_HEIGHT / srcHeight, 1)
  const w = Math.round(srcWidth * scale)
  const h = Math.round(srcHeight * scale)

  const canvas = document.createElement('canvas')
  canvas.width = w
  canvas.height = h
  const ctx = canvas.getContext('2d')
  if (!ctx) return null
  ctx.drawImage(source, 0, 0, w, h)
  return canvas.toDataURL('image/jpeg', 0.7)
}
