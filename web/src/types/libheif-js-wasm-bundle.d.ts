declare module 'libheif-js/wasm-bundle' {
  type DecodedImage = {
    get_width(): number
    get_height(): number
    display(imageData: ImageData, cb: (displayData?: ImageData) => void): void
    free?(): void
  }

  type HeifDecoder = {
    decode(data: Uint8Array): DecodedImage[]
    decoder?: unknown | null
    free?(): void
  }

  type LibheifModule = {
    HeifDecoder: new () => HeifDecoder
    heif_context_free?(context: unknown): void
  }

  const libheif: LibheifModule
  export default libheif
}
