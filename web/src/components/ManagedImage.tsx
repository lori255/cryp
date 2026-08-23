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
  const resolvedSrc = src ?? ''

  useEffect(() => {
    const image = imageRef.current
    if (!image) return

    // React StrictMode may replay an effect without replaying the DOM prop.
    // Restore the source after a development-only cleanup so the replay does
    // not leave a mounted image blank.
    if (image.getAttribute('src') !== resolvedSrc) {
      image.setAttribute('src', resolvedSrc)
    }

    return () => {
      // React may already have committed a newer source before this passive
      // cleanup runs. Only clear the source this effect owns in that case.
      const currentSource = image.getAttribute('src')
      if (currentSource === resolvedSrc || image.src === resolvedSrc) {
        image.removeAttribute('src')
      }
    }
  }, [resolvedSrc])

  return <img ref={imageRef} src={resolvedSrc} loading={loading} {...props} />
}
