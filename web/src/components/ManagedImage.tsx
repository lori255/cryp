import { useEffect, useRef, type ImgHTMLAttributes } from 'react'

/**
 * An image element with an explicit request lifetime.
 *
 * Browsers normally cancel an image request when its node is removed, but
 * clearing the source explicitly also stops pending decode/retry work in
 * WebKit and makes URL changes deterministic.
 */
export default function ManagedImage({ src, loading = 'lazy', ...props }: ImgHTMLAttributes<HTMLImageElement>) {
  const imageRef = useRef<HTMLImageElement | null>(null)

  useEffect(() => {
    const image = imageRef.current
    if (!image) return

    return () => {
      // React may already have committed a newer source before this passive
      // cleanup runs. Only clear the source this effect owns in that case.
      const currentSource = image.getAttribute('src')
      if (currentSource === src || image.src === src) {
        image.removeAttribute('src')
      }
    }
  }, [src])

  return <img ref={imageRef} src={src} loading={loading} {...props} />
}
