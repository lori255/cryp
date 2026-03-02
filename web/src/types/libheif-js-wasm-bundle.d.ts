declare module 'libheif-js/wasm-bundle' {
  type DecodedImage = {
    get_width(): number
    get_height(): number
    display(imageData: ImageData, cb: (displayData?: ImageData) => void): void
  }

  type LibheifModule = {
    HeifDecoder: new () => {
      decode(data: Uint8Array): DecodedImage[]
    }
  }

  const libheif: LibheifModule
  export default libheif
}
