const API_BASE = '/api';

export interface FileItem {
  name: string;
  isDir: boolean;
  size: number;
  modTime: number;
  hasThumb?: boolean;
}

export interface DuplicateFileItem {
  path: string;
  name: string;
  size: number;
  modTime: number;
  hasThumb?: boolean;
}

export interface DuplicateGroup {
  contentHash: string;
  size: number;
  files: DuplicateFileItem[];
}

export interface DuplicateStats {
  groupCount: number;
  fileCount: number;
  duplicateFileCount: number;
  totalBytes: number;
  duplicateTotalBytes: number;
}

export interface TaskRecord {
  id: string;
  vaultId: string;
  type: 'import' | 'upload' | 'index';
  status: 'pending' | 'running' | 'done' | 'error' | 'cancelled';
  totalFiles: number;
  processedFiles: number;
  totalBytes: number;
  processedBytes: number;
  currentFile: string;
  errorMsg?: string;
  sourcePath?: string;
  destPath?: string;
  deleteSource: boolean;
  startedAt: number;
  createdAt: number;
  updatedAt: number;
}

export interface DirEntry {
  name: string;
  isDir: boolean;
  size: number;
}

export interface ApiErrorDetails {
  [key: string]: unknown;
}

export class ApiError extends Error {
  readonly status: number;
  readonly code?: string;
  readonly details?: ApiErrorDetails;

  constructor(status: number, message: string, code?: string, details?: ApiErrorDetails) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
    this.details = details;
  }
}

class ApiClient {
  private sessionId: string | null = null;

  constructor() {
    this.sessionId = typeof localStorage === 'undefined' ? null : localStorage.getItem('sessionId');
  }

  private async request<T = Record<string, unknown>>(path: string, options?: RequestInit): Promise<T> {
    const headers = new Headers(options?.headers);

    if (this.sessionId) {
      headers.set('X-Session-ID', this.sessionId);
    }

    if (options?.body && !(options.body instanceof FormData)) {
      headers.set('Content-Type', 'application/json');
    }

    const res = await fetch(`${API_BASE}${path}`, {
      ...options,
      credentials: 'include',
      headers,
    });

    if (!res.ok) {
      const data = await readResponseBody(res);
      const payload = isRecord(data) ? data : {};
      const message = typeof payload.error === 'string'
        ? payload.error
        : typeof payload.message === 'string'
          ? payload.message
          : `Request failed: ${res.status}`;
      const code = typeof payload.code === 'string' ? payload.code : undefined;
      throw new ApiError(res.status, message, code, payload);
    }

    return await readResponseBody(res) as T;
  }

  async login(name: string, password: string) {
    const data = await this.request<{ sessionId: string; vaultId: string; vaultName: string }>(
      '/auth/login',
      { method: 'POST', body: JSON.stringify({ name, password }) },
    );
    this.setSessionId(data.sessionId);
    return data;
  }

  async logout() {
    await this.request('/auth/logout', { method: 'POST' }).catch(() => {});
    this.setSessionId(null);
  }

  async checkAuth() {
    return this.request<{ authenticated: boolean; sessionId?: string; vaultId?: string; vaultName?: string }>(
      '/auth/status',
    );
  }

  async createVault(name: string, password: string) {
    const data = await this.request<{ id: string; name: string; sessionId: string }>(
      '/vaults',
      { method: 'POST', body: JSON.stringify({ name, password }) },
    );
    this.setSessionId(data.sessionId);
    return data;
  }

  async deleteVault(id: string) {
    return this.request(`/vaults/${id}`, { method: 'DELETE' });
  }

  async listFiles(
    vaultId: string,
    path: string,
    options?: {
      offset?: number
      limit?: number
      sortField?: 'name' | 'modTime' | 'size'
      sortDirection?: 'asc' | 'desc'
      signal?: AbortSignal
    },
  ) {
    const offset = options?.offset ?? 0;
    const limit = options?.limit ?? 100;
    const sortField = options?.sortField ?? 'name';
    const sortDirection = options?.sortDirection ?? 'asc';
    return this.request<{ path: string; files: FileItem[]; hasMore: boolean; nextOffset: number; indexRequired?: boolean }>(
      `/vaults/${vaultId}/files?path=${encodeURIComponent(path)}&offset=${offset}&limit=${limit}&sortField=${sortField}&sortDirection=${sortDirection}`,
      { signal: options?.signal },
    );
  }

  getContentUrl(vaultId: string, path: string): string {
    return `${API_BASE}/vaults/${vaultId}/files/content?path=${encodeURIComponent(path)}`;
  }

  getHlsUrl(vaultId: string, path: string): string {
    return `${API_BASE}/vaults/${vaultId}/files/hls?path=${encodeURIComponent(path)}`;
  }

  getVideoUrl(vaultId: string, path: string): string {
    // Let browsers play codecs they support directly. VideoPlayer promotes
    // an incompatible/failed content response to HLS, which avoids spawning
    // FFmpeg for every ordinary MP4 and reduces pressure on the HLS pool.
    return this.getContentUrl(vaultId, path);
  }

  stopHls(url: string): void {
    const endpoint = this.getHlsStopUrl(url);
    if (!endpoint) return;

    const headers: Record<string, string> = {};
    if (this.sessionId) {
      headers['X-Session-ID'] = this.sessionId;
    }

    // Beacon cannot carry X-Session-ID. Prefer keepalive fetch whenever the
    // client relies on localStorage auth; otherwise use Beacon for its unload
    // reliability and let the HttpOnly cookie authenticate it.
    if (!this.sessionId && typeof navigator !== 'undefined' && navigator.sendBeacon?.(endpoint)) {
      return;
    }

    // A stop may legitimately return 202 while FFmpeg is still unwinding.
    // Retry once so a transient process-group delay is observed by the
    // client, while keeping this best-effort and unload-safe (callers do not
    // need to await the promise from a pagehide/unmount handler).
    void (async () => {
      for (let attempt = 0; attempt < 2; attempt++) {
        let response: Response;
        try {
          response = await fetch(endpoint, {
            method: 'POST',
            credentials: 'include',
            keepalive: true,
            headers,
          });
        } catch {
          if (attempt === 1) return;
          await new Promise((resolve) => setTimeout(resolve, 250));
          continue;
        }
        if (response.status !== 202 || attempt === 1) return;
        const retryAfter = Number(response.headers.get('Retry-After'));
        const delay = Number.isFinite(retryAfter)
          ? Math.max(100, Math.min(2000, retryAfter * 1000))
          : 250;
        await new Promise((resolve) => setTimeout(resolve, delay));
      }
    })();
  }

  private getHlsStopUrl(url: string): string | null {
    let parsed: URL;
    try {
      const origin = typeof window === 'undefined' ? 'http://localhost' : window.location.origin;
      parsed = new URL(url, origin);
    } catch {
      return null;
    }
    const path = parsed.pathname;
    if (path.includes('/files/hls/') && path.endsWith('/index.m3u8')) {
      parsed.pathname = path.replace(/\/index\.m3u8$/, '/stop');
      parsed.search = '';
      return parsed.toString();
    }
    if (path.endsWith('/files/hls')) {
      parsed.pathname = `${path}/stop`;
      return parsed.toString();
    }
    return null;
  }

  getDownloadUrl(vaultId: string, path: string): string {
    return `${API_BASE}/vaults/${vaultId}/files/download?path=${encodeURIComponent(path)}`;
  }

  getThumbnailUrl(vaultId: string, path: string): string {
    return `${API_BASE}/vaults/${vaultId}/thumbnail?path=${encodeURIComponent(path)}`;
  }

  uploadFile(
    vaultId: string,
    currentPath: string,
    file: File,
    onProgress?: (pct: number) => void,
    taskId?: string,
    fileIndex?: number,
    totalFiles?: number,
    signal?: AbortSignal,
  ): Promise<void> {
    return new Promise((resolve, reject) => {
      const xhr = new XMLHttpRequest();
      const abortRequest = () => xhr.abort();
      const cleanup = () => signal?.removeEventListener('abort', abortRequest);
      const formData = new FormData();
      formData.append('file', file);

      xhr.upload.addEventListener('progress', (e) => {
        if (e.lengthComputable && onProgress) {
          onProgress(Math.round((e.loaded / e.total) * 100));
        }
      });

      xhr.addEventListener('load', () => {
        cleanup();
        if (xhr.status >= 200 && xhr.status < 300) {
          resolve();
        } else {
          reject(new ApiError(xhr.status, `Upload failed: ${xhr.status}`));
        }
      });

      xhr.addEventListener('error', () => {
        cleanup();
        reject(new Error('Upload failed'));
      });
      xhr.addEventListener('abort', () => {
        cleanup();
        reject(new DOMException('Upload aborted', 'AbortError'));
      });

      let url = `${API_BASE}/vaults/${vaultId}/files/upload?path=${encodeURIComponent(currentPath)}`;
      if (taskId) {
        url += `&taskId=${taskId}&fileIndex=${fileIndex ?? 0}&totalFiles=${totalFiles ?? 1}`;
      }
      xhr.open('POST', url);
      xhr.withCredentials = true;
      if (this.sessionId) {
        xhr.setRequestHeader('X-Session-ID', this.sessionId);
      }
      if (signal) {
        if (signal.aborted) {
          xhr.abort();
          return;
        }
        signal.addEventListener('abort', abortRequest, { once: true });
      }
      xhr.send(formData);
    });
  }

  async mkdir(vaultId: string, path: string) {
    return this.request(`/vaults/${vaultId}/files/mkdir`, {
      method: 'POST',
      body: JSON.stringify({ path }),
    });
  }

  async deleteFile(vaultId: string, path: string) {
    return this.request(`/vaults/${vaultId}/files?path=${encodeURIComponent(path)}`, {
      method: 'DELETE',
    });
  }

  async deleteFilesBulk(vaultId: string, paths: string[]) {
    return this.request<{ deleted: string[]; failed: Record<string, string> }>(`/vaults/${vaultId}/files/delete-batch`, {
      method: 'POST',
      body: JSON.stringify({ paths }),
    });
  }

  async listDuplicates(vaultId: string, offset = 0, limit = 20, signal?: AbortSignal) {
    return this.request<{ groups: DuplicateGroup[]; hasMore: boolean; nextOffset: number; stats: DuplicateStats; indexRequired?: boolean }>(
      `/vaults/${vaultId}/files/duplicates?offset=${offset}&limit=${limit}`,
      { signal },
    );
  }

  async rebuildFileIndex(vaultId: string) {
    return this.request<{ taskId: string; message: string }>(`/vaults/${vaultId}/files/index/rebuild`, {
      method: 'POST',
    });
  }

  // Directory browsing
  async browseDir(path?: string) {
    const query = path ? `?path=${encodeURIComponent(path)}` : '';
    return this.request<{ path: string; items: DirEntry[] }>(
      `/browse-dir${query}`,
    );
  }

  // Tasks
  async listTasks(vaultId: string, signal?: AbortSignal) {
    return this.request<{ tasks: TaskRecord[] }>(`/vaults/${vaultId}/tasks`, { signal });
  }

  async getTask(vaultId: string, taskId: string) {
    return this.request<TaskRecord>(`/vaults/${vaultId}/tasks/${taskId}`);
  }

  async createImportTask(vaultId: string, sourcePath: string, destPath: string, deleteSource: boolean) {
    return this.request<{ taskId: string }>(`/vaults/${vaultId}/tasks/import`, {
      method: 'POST',
      body: JSON.stringify({ sourcePath, destPath, deleteSource }),
    });
  }

  async createUploadTask(vaultId: string, totalFiles: number, totalBytes: number) {
    return this.request<{ taskId: string }>(`/vaults/${vaultId}/tasks/upload`, {
      method: 'POST',
      body: JSON.stringify({ totalFiles, totalBytes }),
    });
  }

  async cancelTask(vaultId: string, taskId: string) {
    return this.request(`/vaults/${vaultId}/tasks/${taskId}/cancel`, { method: 'POST' });
  }

  async deleteTask(vaultId: string, taskId: string) {
    return this.request(`/vaults/${vaultId}/tasks/${taskId}`, { method: 'DELETE' });
  }

  async deleteCompletedTasks(vaultId: string) {
    return this.request(`/vaults/${vaultId}/tasks/completed`, { method: 'DELETE' });
  }

  getSessionId(): string | null {
    return this.sessionId;
  }

  isLoggedIn(): boolean {
    return this.sessionId !== null;
  }

  private setSessionId(sessionId: string | null): void {
    this.sessionId = sessionId;
    if (typeof localStorage === 'undefined') return;
    if (sessionId) {
      localStorage.setItem('sessionId', sessionId);
    } else {
      localStorage.removeItem('sessionId');
    }
  }
}

export const api = new ApiClient();

export const isImage = (name: string): boolean =>
  /\.(jpg|jpeg|png|gif|webp|bmp|svg|ico|avif|heic|heif)$/i.test(name);

export const isVideo = (name: string): boolean =>
  /\.(mp4|webm|mkv|avi|mov|m4v|flv|wmv|mpg|mpeg|3gp|3g2|ts|mts|m2ts|vob|ogv|asf|rm|rmvb|divx|f4v|mxf|h264|h265|hevc)$/i.test(name);

export const isAudio = (name: string): boolean =>
  /\.(mp3|wav|ogg|flac|aac|m4a|wma)$/i.test(name);

export function joinPath(parent: string, name: string): string {
  if (parent === '/') return `/${name}`;
  return `${parent}/${name}`;
}

export function formatSize(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) return '0 B';
  const k = 1024;
  const sizes = ['B', 'KB', 'MB', 'GB', 'TB'];
  const i = Math.min(sizes.length - 1, Math.floor(Math.log(bytes) / Math.log(k)));
  return `${parseFloat((bytes / Math.pow(k, i)).toFixed(1))} ${sizes[i]}`;
}

export function formatDate(timestamp: number): string {
  if (!timestamp) return '';
  return new Date(timestamp * 1000).toLocaleDateString('zh-CN', {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
  });
}

export function formatETA(startedAt: number, processedBytes: number, totalBytes: number): string {
  if (!Number.isFinite(startedAt) || !Number.isFinite(processedBytes) || !Number.isFinite(totalBytes)
    || processedBytes <= 0 || totalBytes <= 0) return '';
  const elapsed = Math.max(0, Date.now() / 1000 - startedAt);
  const remaining = Math.max(0, (elapsed / processedBytes) * (totalBytes - processedBytes));
  if (remaining < 60) return `预计剩余 ${Math.round(remaining)}s`;
  if (remaining < 3600) {
    const mins = Math.floor(remaining / 60);
    const secs = Math.round(remaining % 60);
    return `预计剩余 ${mins}m ${secs}s`;
  }
  const hours = Math.floor(remaining / 3600);
  const mins = Math.floor((remaining % 3600) / 60);
  return `预计剩余 ${hours}h ${mins}m`;
}

async function readResponseBody(response: Response): Promise<unknown> {
  if (response.status === 204 || response.headers.get('content-length') === '0') {
    return undefined;
  }
  const text = await response.text();
  if (!text.trim()) return undefined;
  const contentType = response.headers.get('content-type') ?? '';
  if (contentType.includes('application/json')) {
    try {
      return JSON.parse(text) as unknown;
    } catch {
      return { error: text };
    }
  }
  try {
    return JSON.parse(text) as unknown;
  } catch {
    return text;
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}
