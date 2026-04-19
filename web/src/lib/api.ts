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

class ApiClient {
  private sessionId: string | null = null;

  constructor() {
    this.sessionId = localStorage.getItem('sessionId');
  }

  private async request<T = Record<string, unknown>>(path: string, options?: RequestInit): Promise<T> {
    const headers: Record<string, string> = {};

    if (this.sessionId) {
      headers['X-Session-ID'] = this.sessionId;
    }

    if (options?.body && !(options.body instanceof FormData)) {
      headers['Content-Type'] = 'application/json';
    }

    const res = await fetch(`${API_BASE}${path}`, {
      ...options,
      credentials: 'include',
      headers: { ...headers, ...(options?.headers as Record<string, string>) },
    });

    if (!res.ok) {
      const data = await res.json().catch(() => ({}));
      throw new Error((data as Record<string, string>).error || `Request failed: ${res.status}`);
    }

    return res.json() as Promise<T>;
  }

  async login(name: string, password: string) {
    const data = await this.request<{ sessionId: string; vaultId: string; vaultName: string }>(
      '/auth/login',
      { method: 'POST', body: JSON.stringify({ name, password }) },
    );
    this.sessionId = data.sessionId;
    localStorage.setItem('sessionId', data.sessionId);
    return data;
  }

  async logout() {
    await this.request('/auth/logout', { method: 'POST' }).catch(() => {});
    this.sessionId = null;
    localStorage.removeItem('sessionId');
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
    this.sessionId = data.sessionId;
    localStorage.setItem('sessionId', data.sessionId);
    return data;
  }

  async deleteVault(id: string) {
    return this.request(`/vaults/${id}`, { method: 'DELETE' });
  }

  async listFiles(
    vaultId: string,
    path: string,
    options?: { offset?: number; limit?: number; sortField?: 'name' | 'modTime' | 'size'; sortDirection?: 'asc' | 'desc' },
  ) {
    const offset = options?.offset ?? 0;
    const limit = options?.limit ?? 100;
    const sortField = options?.sortField ?? 'name';
    const sortDirection = options?.sortDirection ?? 'asc';
    return this.request<{ path: string; files: FileItem[]; hasMore: boolean; nextOffset: number }>(
      `/vaults/${vaultId}/files?path=${encodeURIComponent(path)}&offset=${offset}&limit=${limit}&sortField=${sortField}&sortDirection=${sortDirection}`,
    );
  }

  getContentUrl(vaultId: string, path: string): string {
    return `${API_BASE}/vaults/${vaultId}/files/content?path=${encodeURIComponent(path)}`;
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
  ): Promise<void> {
    return new Promise((resolve, reject) => {
      const xhr = new XMLHttpRequest();
      const formData = new FormData();
      formData.append('file', file);

      xhr.upload.addEventListener('progress', (e) => {
        if (e.lengthComputable && onProgress) {
          onProgress(Math.round((e.loaded / e.total) * 100));
        }
      });

      xhr.addEventListener('load', () => {
        if (xhr.status >= 200 && xhr.status < 300) {
          resolve();
        } else {
          reject(new Error(`Upload failed: ${xhr.status}`));
        }
      });

      xhr.addEventListener('error', () => reject(new Error('Upload failed')));

      let url = `${API_BASE}/vaults/${vaultId}/files/upload?path=${encodeURIComponent(currentPath)}`;
      if (taskId) {
        url += `&taskId=${taskId}&fileIndex=${fileIndex ?? 0}&totalFiles=${totalFiles ?? 1}`;
      }
      xhr.open('POST', url);
      xhr.withCredentials = true;
      if (this.sessionId) {
        xhr.setRequestHeader('X-Session-ID', this.sessionId);
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

  async listDuplicates(vaultId: string, offset = 0, limit = 20) {
    return this.request<{ groups: DuplicateGroup[]; hasMore: boolean; nextOffset: number }>(
      `/vaults/${vaultId}/files/duplicates?offset=${offset}&limit=${limit}`,
    );
  }

  async rebuildFileIndex(vaultId: string) {
    return this.request<{ taskId: string; message: string }>(`/vaults/${vaultId}/files/index/rebuild`, {
      method: 'POST',
    });
  }

  // Directory browsing
  async browseDir(path: string) {
    return this.request<{ path: string; items: DirEntry[] }>(
      `/browse-dir?path=${encodeURIComponent(path)}`,
    );
  }

  // Tasks
  async listTasks(vaultId: string) {
    return this.request<{ tasks: TaskRecord[] }>(`/vaults/${vaultId}/tasks`);
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
}

export const api = new ApiClient();

export const isImage = (name: string): boolean =>
  /\.(jpg|jpeg|png|gif|webp|bmp|svg|ico|avif|heic|heif)$/i.test(name);

export const isVideo = (name: string): boolean =>
  /\.(mp4|webm|mkv|avi|mov|m4v|flv|wmv)$/i.test(name);

export const isAudio = (name: string): boolean =>
  /\.(mp3|wav|ogg|flac|aac|m4a|wma)$/i.test(name);

export function joinPath(parent: string, name: string): string {
  if (parent === '/') return `/${name}`;
  return `${parent}/${name}`;
}

export function formatSize(bytes: number): string {
  if (bytes === 0) return '0 B';
  const k = 1024;
  const sizes = ['B', 'KB', 'MB', 'GB', 'TB'];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
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
  if (processedBytes <= 0 || totalBytes <= 0) return '';
  const elapsed = Date.now() / 1000 - startedAt;
  const remaining = (elapsed / processedBytes) * (totalBytes - processedBytes);
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
