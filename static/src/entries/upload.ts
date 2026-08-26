// Upload page: staging file list, sequential XHR staging with byte progress,
// draft state persistence, SSE-driven job progress, analyze and push actions.

interface WorkspaceFile {
  name: string
  size: string
  modified: string
}

interface WorkspaceState {
  time?: string
  title?: string
  analyzed?: boolean
}

interface JobProgress {
  kind?: string
  message?: string
  file?: string
  done?: number
  total?: number
  done_bytes?: number
  total_bytes?: number
  title?: string
  time?: string
  prefix?: string
  error?: string
}

interface Snapshot {
  lock?: string
  files?: WorkspaceFile[]
  state?: WorkspaceState
  job?: JobProgress
}

function el<T extends HTMLElement>(id: string): T {
  return document.getElementById(id) as T
}

const filesEl = el<HTMLTableSectionElement>('files')
const drop = el<HTMLElement>('drop')
const picker = el<HTMLInputElement>('picker')
const pickerLabel = el<HTMLLabelElement>('picker-label')
const statusEl = el<HTMLElement>('status')
const pushTime = el<HTMLInputElement>('push-time')
const pushTitle = el<HTMLInputElement>('push-title')
const analyzeBtn = document.getElementById('analyze-btn') as HTMLButtonElement | null
const analyzeHint = el<HTMLElement>('analyze-hint')
const pushBtn = el<HTMLButtonElement>('push-btn')
const jobBox = el<HTMLElement>('job-box')
const jobBar = el<HTMLElement>('job-bar')
const jobStatus = el<HTMLElement>('job-status')

let staging = false
let lock = ''
let analyzed = false
const stageMax = 2 * 1024 * 1024 * 1024
let saveTimer: number | undefined

function isBusy(): boolean {
  return staging || !!lock
}

window.addEventListener('beforeunload', (e) => {
  if (isBusy()) e.preventDefault()
})

function fmtBytes(n: number): string {
  if (n < 1024) return n + ' B'
  const units = ['KiB', 'MiB', 'GiB']
  let u = -1
  do {
    n /= 1024
    u++
  } while (n >= 1024 && u < units.length - 1)
  return n.toFixed(1) + ' ' + units[u]
}

function pad2(n: number): string {
  return (n < 10 ? '0' : '') + n
}

function nowLocal(): string {
  const d = new Date()
  return (
    d.getFullYear() + '-' + pad2(d.getMonth() + 1) + '-' + pad2(d.getDate()) +
    'T' + pad2(d.getHours()) + ':' + pad2(d.getMinutes())
  )
}

// The server prefills the pinned staging time; fall back to now only when
// nothing has been pinned yet (empty workspace).
if (!pushTime.value) {
  pushTime.value = nowLocal()
}

function saveState(): void {
  const body = new URLSearchParams()
  body.set('time', pushTime.value)
  body.set('title', pushTitle.value)
  fetch('/upload/state', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
    body: body.toString(),
    credentials: 'same-origin',
  })
    .then((res) => {
      goLoginIfNeeded(res)
    })
    .catch(() => {})
}
pushTime.addEventListener('change', saveState)
pushTitle.addEventListener('change', saveState)
pushTitle.addEventListener('input', () => {
  clearTimeout(saveTimer)
  saveTimer = window.setTimeout(saveState, 400)
})

function showError(msg: string): void {
  if (!msg) {
    statusEl.classList.add('d-none')
    statusEl.textContent = ''
    return
  }
  statusEl.textContent = msg
  statusEl.classList.remove('d-none')
}

function goLoginIfNeeded(res: Response): boolean {
  if (res.redirected && res.url.indexOf('/login') !== -1) {
    location.href = '/login'
    return true
  }
  return false
}

function applyLock(): void {
  const busy = isBusy()
  const jobBusy = lock === 'analyze' || lock === 'push'
  const analyzeRequired = analyzeBtn !== null
  picker.disabled = busy
  pickerLabel.classList.toggle('disabled', busy)
  if (analyzeBtn) analyzeBtn.disabled = busy
  // With the LLM configured, push stays disabled until the staged files went
  // through one successful analyze run (adding/deleting files resets it).
  pushBtn.disabled = busy || (analyzeRequired && !analyzed)
  analyzeHint.classList.toggle('d-none', !analyzeRequired || analyzed)
  pushTime.disabled = jobBusy
  pushTitle.disabled = jobBusy
  drop.classList.toggle('pe-none', busy)
  filesEl.querySelectorAll<HTMLButtonElement>('button[data-name]').forEach((btn) => {
    btn.disabled = busy
  })
}

function applyState(st: WorkspaceState | null): void {
  if (!st) return
  if (document.activeElement !== pushTime) pushTime.value = st.time || nowLocal()
  if (document.activeElement !== pushTitle) pushTitle.value = st.title || ''
  if (typeof st.analyzed === 'boolean') analyzed = st.analyzed
  applyLock()
}

function showJob(): void {
  jobBox.classList.remove('d-none')
}

function setBar(pct: number, animated: boolean, status: string, cls?: string): void {
  showJob()
  if (animated) jobBar.classList.add('progress-bar-animated')
  else jobBar.classList.remove('progress-bar-animated')
  jobBar.style.width = pct + '%'
  jobBar.textContent = pct + '%'
  jobStatus.className = 'small mt-1 mb-0 ' + (cls || 'text-secondary')
  jobStatus.textContent = status
}

function renderJob(job: JobProgress | null): void {
  if (staging) return
  if (!job) {
    if (lock === 'stage') {
      setBar(100, true, 'Staging files…')
    }
    return
  }
  if (job.error) {
    const fail = job.kind === 'analyze' ? 'Analyze failed' : 'Upload failed: ' + job.error
    setBar(parseInt(jobBar.style.width, 10) || 0, false, fail, 'text-danger')
    return
  }
  if (job.kind === 'analyze') {
    let msg = job.message || 'Analyzing…'
    if (job.file) msg += ' (' + job.file + ')'
    const spct = job.total ? Math.floor(((job.done || 0) * 100) / job.total) : 0
    setBar(spct, true, msg)
    return
  }
  if (job.kind === 'push') {
    const pct = job.total_bytes ? Math.floor(((job.done_bytes || 0) * 100) / job.total_bytes) : 0
    const n = (job.done || 0) + (job.file ? 1 : 0)
    let label = 'Uploading ' + n + '/' + job.total
    if (job.file) label += ' (' + job.file + ')'
    if (job.prefix) label += ' to ' + job.prefix
    setBar(pct, true, label)
  }
}

function renderDone(job: JobProgress | null): void {
  if (!job) return
  if (job.kind === 'analyze') {
    if (job.title && document.activeElement !== pushTitle) pushTitle.value = job.title
    if (job.time && document.activeElement !== pushTime) pushTime.value = job.time
    setBar(100, false, 'Analysis complete.', 'text-success')
    return
  }
  if (job.kind === 'push') {
    setBar(100, false, 'Uploaded to ' + (job.prefix || 'bucket'), 'text-success')
  }
}

function render(files: WorkspaceFile[]): void {
  filesEl.replaceChildren()
  if (!files || !files.length) {
    const empty = document.createElement('tr')
    const td = document.createElement('td')
    td.colSpan = 4
    td.className = 'text-secondary'
    td.textContent = 'No files'
    empty.appendChild(td)
    filesEl.appendChild(empty)
    applyLock()
    return
  }
  files.forEach((f) => {
    const tr = document.createElement('tr')
    const nameTd = document.createElement('td')
    const icon = document.createElement('i')
    icon.className = 'bi bi-file-earmark'
    nameTd.appendChild(icon)
    nameTd.appendChild(document.createTextNode(' ' + f.name))
    const sizeTd = document.createElement('td')
    sizeTd.className = 'text-end text-secondary text-nowrap'
    sizeTd.textContent = f.size
    const modTd = document.createElement('td')
    modTd.className = 'text-secondary text-nowrap'
    modTd.textContent = f.modified
    const actTd = document.createElement('td')
    actTd.className = 'text-end'
    const btn = document.createElement('button')
    btn.type = 'button'
    btn.className = 'btn btn-sm btn-outline-danger'
    btn.setAttribute('data-name', f.name)
    btn.innerHTML = '<i class="bi bi-trash"></i> Delete'
    actTd.appendChild(btn)
    tr.appendChild(nameTd)
    tr.appendChild(sizeTd)
    tr.appendChild(modTd)
    tr.appendChild(actTd)
    filesEl.appendChild(tr)
  })
  applyLock()
}

function parseData(e: MessageEvent): any {
  try {
    return JSON.parse(e.data)
  } catch {
    return null
  }
}

const es = new EventSource('/upload/events')
es.addEventListener('snapshot', (e) => {
  const data = parseData(e as MessageEvent) as Snapshot | null
  if (!data) return
  lock = data.lock || ''
  render(data.files || [])
  applyState(data.state || null)
  if (data.job) {
    if (lock) renderJob(data.job)
    else if (data.job.error) renderJob(data.job)
    else renderDone(data.job)
  } else if (lock === 'stage') {
    renderJob(null)
  } else {
    applyLock()
  }
})
es.addEventListener('lock', (e) => {
  const data = parseData(e as MessageEvent)
  if (!data) return
  lock = data.lock || ''
  applyLock()
  if (lock === 'stage' && !staging) {
    renderJob(null)
  } else if (!lock && !staging && jobStatus.textContent === 'Staging files…') {
    jobBox.classList.add('d-none')
  }
})
es.addEventListener('files', (e) => {
  const data = parseData(e as MessageEvent)
  if (data) render(data.files || [])
})
es.addEventListener('state', (e) => {
  applyState(parseData(e as MessageEvent))
})
es.addEventListener('progress', (e) => {
  renderJob(parseData(e as MessageEvent))
})
es.addEventListener('done', (e) => {
  renderDone(parseData(e as MessageEvent))
})
es.addEventListener('error', (e) => {
  if (typeof (e as MessageEvent).data === 'string' && (e as MessageEvent).data) {
    const job = parseData(e as MessageEvent)
    if (job) renderJob(job)
    return
  }
  fetch('/upload/files', { credentials: 'same-origin' })
    .then((res) => {
      goLoginIfNeeded(res)
    })
    .catch(() => {})
})

function uploadOne(file: File, onprogress: (loaded: number, total: number) => void): Promise<void> {
  return new Promise((resolve, reject) => {
    const fd = new FormData()
    fd.append('file', file)
    const xhr = new XMLHttpRequest()
    xhr.open('POST', '/upload/files')
    xhr.upload.onprogress = (ev) => {
      if (ev.lengthComputable) onprogress(ev.loaded, ev.total)
    }
    xhr.onload = () => {
      if (xhr.responseURL && xhr.responseURL.indexOf('/login') !== -1) {
        reject(new Error('login required'))
        location.href = '/login'
        return
      }
      if (xhr.status >= 200 && xhr.status < 300) {
        resolve()
      } else if (xhr.status === 413) {
        reject(new Error('too large (max 2 GiB)'))
      } else if (xhr.status === 409) {
        reject(new Error('workspace busy'))
      } else {
        reject(new Error((xhr.responseText || 'HTTP ' + xhr.status).trim()))
      }
    }
    xhr.onerror = () => {
      reject(new Error('network error'))
    }
    xhr.send(fd)
  })
}

function stageProgress(
  doneBytes: number,
  loaded: number,
  totalBytes: number,
  idx: number,
  count: number,
  name: string,
): void {
  const cur = Math.min(doneBytes + loaded, totalBytes)
  const pct = totalBytes > 0 ? Math.floor((cur * 100) / totalBytes) : 100
  setBar(
    pct,
    true,
    'Uploading ' + idx + '/' + count + ': ' + name +
      ' (' + fmtBytes(cur) + ' / ' + fmtBytes(totalBytes) + ')',
  )
}

function uploadFiles(list: FileList | File[] | null, dirRejected?: boolean): void {
  if (!list) return
  if (isBusy()) {
    showError('Another operation is in progress, try again in a moment.')
    picker.value = ''
    return
  }
  const files: File[] = []
  let rejected = !!dirRejected
  const tooLarge: string[] = []
  for (let i = 0; i < list.length; i++) {
    const f = list[i]
    const name = f.name || ''
    if (!name || name.charAt(0) === '.' || f.webkitRelativePath) {
      rejected = true
    } else if (f.size >= stageMax) {
      tooLarge.push(name)
    } else {
      files.push(f)
    }
  }
  if (!files.length) {
    if (rejected) showError('Folders and hidden files (names starting with ".") are not allowed.')
    if (tooLarge.length) showError('Over the max 2 GiB, skipped: ' + tooLarge.join(', '))
    picker.value = ''
    return
  }
  staging = true
  applyLock()
  showError('')
  let totalBytes = 0
  files.forEach((f) => {
    totalBytes += f.size
  })
  let doneBytes = 0
  const failed: string[] = []
  let idx = 0
  function finish(): void {
    staging = false
    applyLock()
    picker.value = ''
    if (!failed.length) {
      setBar(100, false, 'Staged ' + files.length + '/' + files.length + ' file(s).')
    } else {
      setBar(
        parseInt(jobBar.style.width, 10) || 0,
        false,
        'Staged ' + (files.length - failed.length) + '/' + files.length + ' file(s).',
      )
    }
    const msgs: string[] = []
    if (failed.length) msgs.push('Failed: ' + failed.join('; '))
    if (rejected) msgs.push('Folders and hidden files were skipped.')
    if (tooLarge.length) msgs.push('Over the max 2 GiB, skipped: ' + tooLarge.join(', '))
    if (msgs.length) showError(msgs.join(' '))
  }
  function next(): void {
    if (idx >= files.length) {
      finish()
      return
    }
    const f = files[idx]
    const base = doneBytes
    idx++
    stageProgress(base, 0, totalBytes, idx, files.length, f.name)
    uploadOne(f, (loaded) => {
      stageProgress(base, loaded, totalBytes, idx, files.length, f.name)
    })
      .then(() => {
        doneBytes += f.size
        next()
      })
      .catch((err: Error) => {
        failed.push(f.name + ' (' + err.message + ')')
        next()
      })
  }
  next()
}

function deleteFile(name: string | null): void {
  if (!name || isBusy()) return
  if (!confirm('Delete ' + name + '?')) return
  fetch('/upload/files?name=' + encodeURIComponent(name), {
    method: 'DELETE',
    credentials: 'same-origin',
  })
    .then((res) => {
      if (goLoginIfNeeded(res)) return
      if (res.status === 409) throw new Error('Another operation is in progress, try again in a moment.')
      if (!res.ok) throw new Error('delete failed')
    })
    .catch((err: Error) => {
      showError(err.message || 'Delete failed.')
    })
}

if (analyzeBtn) {
  analyzeBtn.addEventListener('click', () => {
    if (analyzeBtn.disabled || isBusy()) return
    showError('')
    fetch('/upload/analyze', { method: 'POST', credentials: 'same-origin' })
      .then((res) => {
        if (goLoginIfNeeded(res)) return
        if (res.status === 409) throw new Error('Another operation is in progress, try again in a moment.')
        if (!res.ok) {
          return res.text().then((t) => {
            throw new Error((t || 'analyze failed').trim())
          })
        }
      })
      .catch((err: Error) => {
        showError(err.message || 'Could not analyze the staged files.')
      })
  })
}

pushBtn.addEventListener('click', () => {
  if (pushBtn.disabled || isBusy()) return
  const body = new URLSearchParams()
  body.set('time', pushTime.value)
  body.set('title', pushTitle.value)
  showError('')
  fetch('/upload/push', {
    method: 'POST',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
    body: body.toString(),
    credentials: 'same-origin',
  })
    .then((res) => {
      if (goLoginIfNeeded(res)) return
      if (res.status === 409) throw new Error('Another operation is in progress, try again in a moment.')
      if (!res.ok) {
        return res.text().then((t) => {
          throw new Error((t || 'push failed').trim())
        })
      }
    })
    .catch((err: Error) => {
      showError(err.message || 'Could not start the upload.')
    })
})

picker.addEventListener('change', () => {
  uploadFiles(picker.files)
})
drop.addEventListener('dragover', (e) => {
  e.preventDefault()
  if (!isBusy()) drop.classList.add('border-primary')
})
drop.addEventListener('dragleave', () => {
  drop.classList.remove('border-primary')
})
drop.addEventListener('drop', (e) => {
  e.preventDefault()
  drop.classList.remove('border-primary')
  let files: File[] = []
  let dirRejected = false
  const items = e.dataTransfer?.items
  if (items && items.length) {
    for (let i = 0; i < items.length; i++) {
      const item = items[i]
      if (item.kind !== 'file') {
        dirRejected = true
        continue
      }
      const entry = item.webkitGetAsEntry ? item.webkitGetAsEntry() : null
      if (entry && entry.isDirectory) {
        dirRejected = true
        continue
      }
      const f = item.getAsFile()
      if (f) files.push(f)
    }
  } else {
    files = Array.prototype.slice.call(e.dataTransfer?.files || [])
  }
  uploadFiles(files, dirRejected)
})
filesEl.addEventListener('click', (e) => {
  const btn = (e.target as HTMLElement | null)?.closest('button[data-name]')
  if (btn) deleteFile(btn.getAttribute('data-name'))
})
applyLock()
