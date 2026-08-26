declare module 'artplayer' {
  interface ArtplayerOptions {
    container: HTMLElement;
    url: string;
    [key: string]: unknown;
  }

  export default class Artplayer {
    constructor(options: ArtplayerOptions);
    destroy(removeHtml?: boolean): void;
    video: HTMLVideoElement;
    notice: { show: string };
    on(event: string, callback: (...args: unknown[]) => void): void;
    pause(): void;
    play(): void;
  }
}
