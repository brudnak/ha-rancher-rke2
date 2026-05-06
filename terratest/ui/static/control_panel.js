const setupData = JSON.parse(document.getElementById('control-panel-data')?.textContent || '{}')
const token = setupData.token || ''

const clustersSectionEl = document.getElementById('clustersSection')
const clustersEl = document.getElementById('clusters')
const refreshStatusEl = document.getElementById('refreshStatus')
const logStatusEl = document.getElementById('logStatus')
const logBoxEl = document.getElementById('logBox')
const logModalEl = document.getElementById('logModal')
const logModalKindEl = document.getElementById('logModalKind')
const logModalTitleEl = document.getElementById('logModalTitle')
const logModalSubtitleEl = document.getElementById('logModalSubtitle')
const logSearchEl = document.getElementById('logSearch')
const logMatchCountEl = document.getElementById('logMatchCount')
const logLevelFiltersEl = document.getElementById('logLevelFilters')
const liveLogStateEl = document.getElementById('liveLogState')
const liveLogStateIconEl = document.getElementById('liveLogStateIcon')
const liveLogStateLabelEl = document.getElementById('liveLogStateLabel')
const openLogViewerBtnEl = document.getElementById('openLogViewerBtn')
const stopStreamBtnEl = document.getElementById('stopStreamBtn')
const cleanupStatusEl = document.getElementById('cleanupStatus')
const cleanupActionsEl = document.getElementById('cleanupActions')
const cleanupConfirmEl = document.getElementById('cleanupConfirm')
const cleanupBtnEl = document.getElementById('cleanupBtn')
const openCleanupLogsBtnEl = document.getElementById('openCleanupLogsBtn')
const cleanupCostEl = document.getElementById('cleanupCost')
const themeToggleEl = document.getElementById('themeToggle')
const themeSunIconEl = document.getElementById('themeSunIcon')
const themeMoonIconEl = document.getElementById('themeMoonIcon')
const themeToggleLabelEl = document.getElementById('themeToggleLabel')

let stream = null
let previousLeaders = new Map()
let pendingLeaderHighlights = new Map()
let collapsedClusters = new Map()
let collapsedPods = new Map()
let initializedCollapseState = new Set()
let lastState = null
let activeDownloadClusterId = ''
let activeCopyClusterId = ''
let lastLeaderChangeMessage = ''
let refreshInFlight = false
let rawLogText = ''
let visibleLogText = ''
let activeLogContext = null
let activeLogLevel = 'all'
let liveLogState = 'idle'

const currentTheme = () => document.documentElement.classList.contains('dark') ? 'dark' : 'light'

const setTheme = theme => {
  document.documentElement.classList.toggle('dark', theme === 'dark')
  document.body.classList.toggle('dark', theme === 'dark')
  localStorage.setItem('rancherControlPanelTheme', theme)

  themeSunIconEl.classList.toggle('hidden', theme !== 'dark')
  themeMoonIconEl.classList.toggle('hidden', theme !== 'light')
  themeToggleLabelEl.textContent = theme === 'dark' ? 'Light' : 'Dark'
}

const escapeHtml = value => String(value || '')
  .replaceAll('&', '&amp;')
  .replaceAll('<', '&lt;')
  .replaceAll('>', '&gt;')
  .replaceAll('"', '&quot;')
  .replaceAll('\'', '&#39;')

const escapeRegExp = value => String(value || '').replace(/[.*+?^${}()|[\]\\]/g, '\\$&')

const highlightLogLine = (line, query) => {
  const escapedLine = escapeHtml(line)
  if (!query) {
    return escapedLine || '&nbsp;'
  }

  const pattern = new RegExp(escapeRegExp(query), 'ig')
  const highlighted = escapedLine.replace(pattern, match => `<mark class="rounded bg-amber-200 px-0.5 text-zinc-950 dark:bg-amber-300">${match}</mark>`)
  return highlighted || '&nbsp;'
}

const lineMatchesLogLevel = (line, level) => {
  if (level === 'all') {
    return true
  }

  const patterns = {
    info: /\b(info|level=info|level="info")\b/i,
    debug: /\b(debug|level=debug|level="debug")\b/i,
    warning: /\b(warn|warning|level=warn|level=warning|level="warn"|level="warning")\b/i,
    error: /\b(error|err|level=error|level=err|level="error"|level="err")\b/i
  }

  return patterns[level]?.test(line) || false
}

const extractCleanupLineValue = (output, label) => {
  const line = output.find(item => item.includes(label))
  if (!line) {
    return ''
  }

  return line.slice(line.indexOf(label) + label.length).trim()
}

const parseCleanupCost = output => {
  const total = extractCleanupLineValue(output, 'Estimated total (EC2 + EBS only):')
  if (!total) {
    return null
  }

  return {
    total,
    region: extractCleanupLineValue(output, 'Region:'),
    runtime: extractCleanupLineValue(output, 'Total runtime across instances:'),
    ec2: extractCleanupLineValue(output, 'EC2:'),
    ebs: extractCleanupLineValue(output, 'EBS:')
  }
}

const setActiveLogLevel = level => {
  activeLogLevel = level
  logLevelFiltersEl.querySelectorAll('button[data-level]').forEach(button => {
    const active = button.dataset.level === level
    button.className = active
      ? 'rounded-full border border-emerald-200 bg-emerald-50 px-3 py-1.5 text-xs font-semibold text-emerald-700 dark:border-emerald-500/30 dark:bg-emerald-500/15 dark:text-emerald-300'
      : 'rounded-full border border-zinc-200 bg-white px-3 py-1.5 text-xs font-semibold text-zinc-600 hover:bg-zinc-50 dark:border-white/10 dark:bg-white/[0.06] dark:text-zinc-300 dark:hover:bg-white/[0.1]'
  })
  renderLogViewer()
}

const setLiveLogState = state => {
  liveLogState = state

  const states = {
    idle: {
      label: 'Idle',
      container: 'border-zinc-200 bg-zinc-50 text-zinc-500 dark:border-white/10 dark:bg-white/[0.06] dark:text-zinc-400',
      icon: 'bg-zinc-400',
      button: 'Start live'
    },
    connecting: {
      label: 'Connecting to live logs...',
      container: 'border-sky-200 bg-sky-50 text-sky-700 dark:border-sky-500/30 dark:bg-sky-500/15 dark:text-sky-300',
      icon: 'bg-sky-500 animate-ping',
      button: 'Stop live'
    },
    live: {
      label: 'Live stream connected',
      container: 'border-emerald-200 bg-emerald-50 text-emerald-700 dark:border-emerald-500/30 dark:bg-emerald-500/15 dark:text-emerald-300',
      icon: 'bg-emerald-500 animate-pulse',
      button: 'Stop live'
    },
    stopped: {
      label: 'Live stream stopped',
      container: 'border-zinc-200 bg-zinc-50 text-zinc-600 dark:border-white/10 dark:bg-white/[0.06] dark:text-zinc-300',
      icon: 'bg-zinc-400',
      button: 'Resume live'
    },
    error: {
      label: 'Live stream interrupted',
      container: 'border-rose-200 bg-rose-50 text-rose-700 dark:border-rose-500/30 dark:bg-rose-500/15 dark:text-rose-300',
      icon: 'bg-rose-500',
      button: 'Resume live'
    },
    cleanupRunning: {
      label: 'Cleanup running',
      container: 'border-sky-200 bg-sky-50 text-sky-700 dark:border-sky-500/30 dark:bg-sky-500/15 dark:text-sky-300',
      icon: 'bg-sky-500 animate-pulse',
      button: 'Live disabled'
    },
    cleanupDone: {
      label: 'Cleanup completed',
      container: 'border-emerald-200 bg-emerald-50 text-emerald-700 dark:border-emerald-500/30 dark:bg-emerald-500/15 dark:text-emerald-300',
      icon: 'bg-emerald-500',
      button: 'Live disabled'
    },
    cleanupError: {
      label: 'Cleanup failed',
      container: 'border-rose-200 bg-rose-50 text-rose-700 dark:border-rose-500/30 dark:bg-rose-500/15 dark:text-rose-300',
      icon: 'bg-rose-500',
      button: 'Live disabled'
    }
  }
  const selected = states[state] || states.idle

  liveLogStateEl.className = `mt-3 inline-flex items-center gap-2 rounded-full border px-3 py-1.5 text-xs font-semibold ${selected.container}`
  liveLogStateIconEl.className = `h-2.5 w-2.5 rounded-full ${selected.icon}`
  liveLogStateLabelEl.textContent = selected.label
  stopStreamBtnEl.textContent = selected.button
  stopStreamBtnEl.classList.toggle('hidden', state.startsWith('cleanup'))
}

const logFilename = () => {
  if (activeLogContext?.mode === 'cleanup') {
    const filter = logSearchEl.value.trim() ? '-filtered' : ''
    return `cleanup${filter}.log`
  }

  const pod = activeLogContext?.podName || 'pod'
  const mode = activeLogContext?.mode || 'logs'
  const filter = logSearchEl.value.trim() ? '-filtered' : ''
  const safePod = pod.toLowerCase().replace(/[^a-z0-9._-]+/g, '-').replace(/^-+|-+$/g, '') || 'pod'
  return `${safePod}-${mode}${filter}.log`
}

const openLogModal = () => {
  logModalEl.classList.remove('hidden')
  document.body.classList.add('overflow-hidden')
}

const closeLogModal = () => {
  logModalEl.classList.add('hidden')
  document.body.classList.remove('overflow-hidden')
}

const setActiveLogContext = (mode, clusterId, namespace, podName) => {
  activeLogContext = { mode, clusterId, namespace, podName }
  logModalKindEl.textContent = 'Pod logs'
  logModalTitleEl.textContent = podName
  logModalSubtitleEl.textContent = `${namespace} • ${clusterId} • ${mode === 'live' ? 'live stream' : 'tail snapshot'}`
  openLogViewerBtnEl.classList.remove('hidden')
}

const setCleanupLogContext = () => {
  activeLogContext = { mode: 'cleanup', clusterId: 'local', namespace: 'terratest', podName: 'cleanup' }
  logModalKindEl.textContent = 'Cleanup logs'
  logModalTitleEl.textContent = 'Cleanup'
  logModalSubtitleEl.textContent = 'go test -v -run TestHACleanup -timeout 20m ./terratest'
  openLogViewerBtnEl.classList.remove('hidden')
}

const renderLogViewer = () => {
  const query = logSearchEl.value.trim()
  const entries = rawLogText.split('\n').map((line, index) => ({ line, index: index + 1 }))
  const filteredEntries = entries.filter(entry => {
    const queryMatches = query ? entry.line.toLowerCase().includes(query.toLowerCase()) : true
    return queryMatches && lineMatchesLogLevel(entry.line, activeLogLevel)
  })

  visibleLogText = filteredEntries.map(entry => entry.line).join('\n')
  const filterLabel = activeLogLevel === 'all' ? '' : ` • ${activeLogLevel.toUpperCase()}`
  logMatchCountEl.textContent = query || activeLogLevel !== 'all'
    ? `${filteredEntries.length} of ${entries.length} lines${filterLabel}`
    : `${entries.length} lines`

  if (!filteredEntries.length || (filteredEntries.length === 1 && filteredEntries[0].line === '')) {
    const waitingForLive = activeLogContext?.mode === 'live' && (liveLogState === 'connecting' || liveLogState === 'live')
    const waitingForCleanup = activeLogContext?.mode === 'cleanup' && liveLogState === 'cleanupRunning'
    const waiting = waitingForLive || waitingForCleanup
    logBoxEl.innerHTML = `
      <div class="flex h-full min-h-64 items-center justify-center rounded-xl border border-dashed border-zinc-300 bg-white text-sm text-zinc-500 dark:border-white/10 dark:bg-white/[0.03] dark:text-zinc-400">
        <div class="flex items-center gap-3">
          ${waiting ? '<span class="spinner"></span>' : ''}
          <span>${waitingForLive ? 'Waiting for live log lines...' : waitingForCleanup ? 'Waiting for cleanup output...' : query || activeLogLevel !== 'all' ? 'No matching log lines.' : 'No logs loaded yet.'}</span>
        </div>
      </div>
    `
    return
  }

  logBoxEl.innerHTML = filteredEntries.map(entry => `
    <div class="grid grid-cols-[4.5rem_minmax(0,1fr)] border-b border-zinc-200/70 bg-white/60 last:border-b-0 dark:border-white/5 dark:bg-white/[0.02]">
      <div class="select-none px-3 py-1.5 text-right text-[11px] tabular-nums text-zinc-400 dark:text-zinc-600">${entry.index}</div>
      <code class="min-w-0 whitespace-pre-wrap break-words px-3 py-1.5 text-zinc-800 dark:text-zinc-200">${highlightLogLine(entry.line, query)}</code>
    </div>
  `).join('')
}

const appendLogLine = line => {
  if (activeLogContext?.mode === 'live' && liveLogState !== 'live') {
    setLiveLogState('live')
  }
  rawLogText = rawLogText ? `${rawLogText}\n${line}` : line
  renderLogViewer()
  if (!logSearchEl.value.trim()) {
    logBoxEl.scrollTop = logBoxEl.scrollHeight
  }
}

const clusterItems = state => state && state.clusters && Array.isArray(state.clusters.items)
  ? state.clusters.items
  : []

const podsFor = cluster => Array.isArray(cluster.pods) ? cluster.pods : []

const statusFor = cluster => {
  if (cluster.reachable) {
    return {
      label: 'Reachable',
      className: 'bg-emerald-100 text-emerald-700 dark:bg-emerald-500/15 dark:text-emerald-300'
    }
  }
  if (cluster.provisioning) {
    return {
      label: 'Provisioning',
      className: 'bg-amber-100 text-amber-700 dark:bg-amber-500/15 dark:text-amber-300'
    }
  }
  if (cluster.available) {
    return {
      label: 'Unavailable',
      className: 'bg-amber-100 text-amber-700 dark:bg-amber-500/15 dark:text-amber-300'
    }
  }
  return {
    label: 'Missing',
    className: 'bg-rose-100 text-rose-700 dark:bg-rose-500/15 dark:text-rose-300'
  }
}

const initializeCollapseState = cluster => {
  if (initializedCollapseState.has(cluster.id)) {
    return
  }

  initializedCollapseState.add(cluster.id)
  if (cluster.type === 'downstream') {
    collapsedClusters.set(cluster.id, true)
    collapsedPods.set(cluster.id, true)
  }
}

const emptyPodsText = cluster => cluster.type === 'downstream'
  ? 'No pods found in the downstream cluster yet.'
  : 'No Rancher/webhook pods found in cattle-system.'

const fetchState = async () => {
  const response = await fetch('/api/state', {
    cache: 'no-store',
    headers: {
      'Accept': 'application/json',
      'X-Control-Panel-Token': token
    }
  })

  if (!response.ok) {
    throw new Error(await response.text() || 'Failed to fetch state')
  }

  return response.json()
}

const badge = label => `<span class="inline-flex items-center rounded-md bg-zinc-100 px-2 py-1 text-xs font-semibold text-zinc-600 dark:bg-white/[0.06] dark:text-zinc-300">${escapeHtml(label)}</span>`

const metaItem = (label, value) => `
  <div class="min-w-0">
    <div class="text-xs font-semibold uppercase tracking-wide text-zinc-500 dark:text-zinc-400">${escapeHtml(label)}</div>
    <div class="mt-1 break-words text-sm font-medium text-zinc-800 [overflow-wrap:anywhere] dark:text-zinc-200">${value}</div>
  </div>
`

const renderKubeconfigActions = cluster => {
  if (!cluster.available) {
    return '<span class="text-sm text-zinc-500 dark:text-zinc-400">Kubeconfig unavailable</span>'
  }

  const downloading = activeDownloadClusterId === cluster.id
  const copying = activeCopyClusterId === cluster.id
  const downloadSpinner = downloading ? '<span class="spinner mr-2"></span>' : ''
  const copySpinner = copying ? '<span class="spinner mr-2"></span>' : ''
  const downloadLabel = cluster.type === 'downstream' ? 'Download downstream kubeconfig' : 'Download kubeconfig'

  return `
    <button type="button" data-action="download" data-cluster="${escapeHtml(cluster.id)}"${downloading ? ' disabled' : ''} class="inline-flex min-h-11 max-w-full items-center justify-center whitespace-normal rounded-lg bg-emerald-500 px-4 py-2 text-center text-sm font-semibold text-white shadow-sm shadow-emerald-500/20 hover:bg-emerald-400 disabled:cursor-default disabled:opacity-70">${downloadSpinner}${downloading ? 'Preparing kubeconfig' : downloadLabel}</button>
    <button type="button" data-action="copy-kubeconfig" data-cluster="${escapeHtml(cluster.id)}"${copying ? ' disabled' : ''} class="inline-flex min-h-11 items-center justify-center rounded-lg border border-zinc-200 bg-white px-4 py-2 text-sm font-semibold text-zinc-700 hover:bg-zinc-50 disabled:cursor-default disabled:opacity-70 dark:border-white/10 dark:bg-white/[0.06] dark:text-zinc-200 dark:hover:bg-white/[0.1]">${copySpinner}${copying ? 'Copying' : 'Copy kubeconfig'}</button>
  `
}

const renderPodRows = (cluster, pods, changedLeader) => {
  if (!pods.length) {
    const message = cluster.error ? cluster.error : emptyPodsText(cluster)
    return `<tr><td colspan="8" class="px-3 py-4 text-sm text-zinc-500 dark:text-zinc-400">${escapeHtml(message)}</td></tr>`
  }

  return pods.map(pod => {
    const rowClass = changedLeader && changedLeader === pod.name
      ? 'bg-emerald-50 dark:bg-emerald-500/10'
      : pod.leader
        ? 'bg-emerald-50/70 dark:bg-emerald-500/5'
        : ''
    const leaderBadge = pod.leader && pod.leaderLabel ? badge(pod.leaderLabel) : ''

    return `
      <tr class="${rowClass}">
        <td class="break-words px-3 py-3 align-top text-sm text-zinc-600 dark:text-zinc-400">${escapeHtml(pod.namespace || '')}</td>
        <td class="px-3 py-3 align-top">
          <div class="flex flex-wrap items-center gap-2 text-sm font-semibold text-zinc-900 dark:text-zinc-100">
            <span>${escapeHtml(pod.name)}</span>
            ${leaderBadge}
          </div>
          <div class="mt-2 flex flex-wrap gap-2">
            <button type="button" data-action="tail" data-cluster="${escapeHtml(cluster.id)}" data-namespace="${escapeHtml(pod.namespace || 'cattle-system')}" data-pod="${escapeHtml(pod.name)}" class="rounded-lg border border-zinc-200 bg-white px-3 py-1.5 text-xs font-semibold text-zinc-700 hover:bg-zinc-50 dark:border-white/10 dark:bg-white/[0.06] dark:text-zinc-200 dark:hover:bg-white/[0.1]">Tail</button>
            <button type="button" data-action="live" data-cluster="${escapeHtml(cluster.id)}" data-namespace="${escapeHtml(pod.namespace || 'cattle-system')}" data-pod="${escapeHtml(pod.name)}" class="rounded-lg border border-zinc-200 bg-white px-3 py-1.5 text-xs font-semibold text-zinc-700 hover:bg-zinc-50 dark:border-white/10 dark:bg-white/[0.06] dark:text-zinc-200 dark:hover:bg-white/[0.1]">Live</button>
          </div>
        </td>
        <td class="break-words px-3 py-3 align-top text-sm text-zinc-700 dark:text-zinc-300">${escapeHtml(pod.ready)}</td>
        <td class="break-words px-3 py-3 align-top text-sm text-zinc-700 dark:text-zinc-300">${escapeHtml(pod.status)}</td>
        <td class="break-words px-3 py-3 align-top text-sm text-zinc-700 dark:text-zinc-300">${pod.restarts}</td>
        <td class="break-words px-3 py-3 align-top text-sm text-zinc-700 dark:text-zinc-300">${escapeHtml(pod.age)}</td>
        <td class="break-words px-3 py-3 align-top text-sm text-zinc-700 dark:text-zinc-300">${escapeHtml(pod.node || '')}</td>
        <td class="break-words px-3 py-3 align-top text-sm text-zinc-700 dark:text-zinc-300">${escapeHtml(pod.containers)}</td>
      </tr>
    `
  }).join('')
}

const renderPodsTable = (cluster, pods, changedLeader) => {
  const podsCollapsed = collapsedPods.get(cluster.id) === true
  const toggleText = podsCollapsed ? 'Show pods' : 'Hide pods'

  if (cluster.provisioning) {
    return `
      <div class="mt-4 rounded-xl border border-amber-200 bg-amber-50 px-4 py-3 text-sm font-medium text-amber-800 dark:border-amber-500/20 dark:bg-amber-500/10 dark:text-amber-200">
        <span class="spinner mr-2"></span>${escapeHtml(cluster.provisioningMessage || 'Provisioning downstream cluster')}
      </div>
    `
  }

  return `
    <div class="mt-4 flex items-center justify-between gap-3">
      <div class="text-sm font-semibold text-zinc-950 dark:text-zinc-100">Pods <span class="text-zinc-500 dark:text-zinc-400">${pods.length}</span></div>
      <button type="button" data-action="toggle-pods" data-cluster="${escapeHtml(cluster.id)}" class="rounded-lg border border-zinc-200 bg-white px-3 py-2 text-xs font-semibold text-zinc-700 hover:bg-zinc-50 dark:border-white/10 dark:bg-white/[0.06] dark:text-zinc-200 dark:hover:bg-white/[0.1]">${toggleText}</button>
    </div>
    ${podsCollapsed ? '' : `
      <div class="mt-3 max-w-full overflow-hidden rounded-xl border border-zinc-200 dark:border-white/10">
        <table class="w-full table-fixed border-collapse text-left">
          <colgroup>
            <col class="w-[9rem]" />
            <col class="w-[24rem]" />
            <col class="w-[5rem]" />
            <col class="w-[7rem]" />
            <col class="w-[6rem]" />
            <col class="w-[5rem]" />
            <col class="w-[12rem]" />
            <col />
          </colgroup>
          <thead class="bg-zinc-50 dark:bg-white/[0.04]">
            <tr>
              ${['Namespace', 'Pod', 'Ready', 'Status', 'Restarts', 'Age', 'Node', 'Containers'].map(label => `<th class="px-3 py-2 text-xs font-semibold uppercase tracking-wide text-zinc-500 dark:text-zinc-400">${label}</th>`).join('')}
            </tr>
          </thead>
          <tbody class="divide-y divide-zinc-200 dark:divide-white/10">
            ${renderPodRows(cluster, pods, changedLeader)}
          </tbody>
        </table>
      </div>
    `}
  `
}

const renderCluster = cluster => {
  initializeCollapseState(cluster)

  const pods = podsFor(cluster)
  const currentLeader = pods.find(pod => pod.leader && pod.leaderLabel === 'Leader') || pods.find(pod => pod.leader)
  const changedLeader = pendingLeaderHighlights.get(cluster.id)
  const isDownstream = cluster.type === 'downstream'
  const status = statusFor(cluster)
  const clusterCollapsed = collapsedClusters.get(cluster.id) === true
  const toggleText = clusterCollapsed ? 'Show details' : 'Hide details'
  const version = cluster.version ? ` <span class="text-zinc-500 dark:text-zinc-400">(${escapeHtml(cluster.version)})</span>` : ''
  const typeBadge = badge(isDownstream ? 'Downstream' : 'Local')
  const contextParts = isDownstream
    ? [`Downstream from HA ${cluster.haIndex}`]
    : [`Management cluster for HA ${cluster.haIndex}`]

  if (isDownstream && cluster.namespace) {
    contextParts.push(`namespace ${cluster.namespace}`)
  }
  if (isDownstream && cluster.managementClusterId) {
    contextParts.push(cluster.managementClusterId)
  }

  const rancherURL = cluster.rancherUrl
    ? `<a href="${escapeHtml(cluster.rancherUrl)}" target="_blank" rel="noreferrer" class="text-emerald-600 hover:text-emerald-500 dark:text-emerald-300">${escapeHtml(cluster.rancherUrl)}</a>`
    : '<span class="text-zinc-500 dark:text-zinc-400">Unavailable</span>'
  const loadBalancer = cluster.loadBalancer ? escapeHtml(cluster.loadBalancer) : '<span class="text-zinc-500 dark:text-zinc-400">Unavailable</span>'
  const kubeconfig = cluster.kubeconfigPath ? escapeHtml(cluster.kubeconfigPath) : '<span class="text-zinc-500 dark:text-zinc-400">Generated on download</span>'
  const namespace = cluster.namespace ? metaItem('Namespace', escapeHtml(cluster.namespace)) : ''
  const clusterID = cluster.managementClusterId ? metaItem('Cluster ID', escapeHtml(cluster.managementClusterId)) : ''
  const leaderSummary = currentLeader
    ? `<div class="mt-4 text-sm text-zinc-600 dark:text-zinc-400"><strong class="text-zinc-950 dark:text-zinc-100">Active Leader</strong> ${escapeHtml(currentLeader.name)}</div>`
    : '<div class="mt-4 text-sm text-zinc-500 dark:text-zinc-400">Leader not detected yet.</div>'
  const downstreamClasses = isDownstream
    ? 'border-l-4 border-l-emerald-500 bg-emerald-50/50 dark:bg-emerald-500/[0.04]'
    : 'bg-white dark:bg-white/[0.03]'

  return `
    <article class="min-w-0 overflow-hidden rounded-2xl border border-zinc-200 ${downstreamClasses} p-4 shadow-sm dark:border-white/10">
      <div class="flex min-w-0 flex-col gap-4 lg:flex-row lg:items-start lg:justify-between">
        <div class="min-w-0">
          <div class="flex flex-wrap items-center gap-2 text-lg font-semibold tracking-tight text-zinc-950 dark:text-zinc-50">
            <span>${escapeHtml(cluster.name)}${version}</span>
            ${typeBadge}
          </div>
          <div class="mt-1 break-words text-sm font-medium text-zinc-500 dark:text-zinc-400">${escapeHtml(contextParts.join(' • '))}</div>
        </div>
        <div class="flex min-w-0 flex-wrap items-center gap-2 lg:max-w-sm lg:justify-end">
          ${renderKubeconfigActions(cluster)}
          <button type="button" data-action="toggle-cluster" data-cluster="${escapeHtml(cluster.id)}" class="rounded-lg border border-zinc-200 bg-white px-3 py-2 text-sm font-semibold text-zinc-700 hover:bg-zinc-50 dark:border-white/10 dark:bg-white/[0.06] dark:text-zinc-200 dark:hover:bg-white/[0.1]">${toggleText}</button>
          <span class="inline-flex items-center rounded-full px-3 py-1.5 text-xs font-semibold ${status.className}">${cluster.provisioning ? '<span class="spinner mr-2"></span>' : ''}${status.label}</span>
        </div>
      </div>
      ${clusterCollapsed ? '' : `
        <div class="mt-4 grid min-w-0 gap-3 sm:grid-cols-2 xl:grid-cols-[repeat(3,minmax(0,1fr))]">
          ${metaItem('Rancher URL', rancherURL)}
          ${metaItem('Load Balancer', loadBalancer)}
          ${metaItem('Kubeconfig', kubeconfig)}
          ${namespace}
          ${clusterID}
        </div>
        ${leaderSummary}
        ${renderPodsTable(cluster, pods, changedLeader)}
      `}
    </article>
  `
}

const renderClusters = state => {
  const cleanup = state?.cleanup || {}

  if (cleanup.finishedAt && !cleanup.error) {
    clustersSectionEl.classList.add('hidden')
    clustersEl.innerHTML = ''
    return
  }

  clustersSectionEl.classList.remove('hidden')

  if (cleanup.running) {
    clustersEl.innerHTML = `
      <div class="rounded-2xl border border-sky-200 bg-sky-50 p-6 text-center dark:border-sky-500/20 dark:bg-sky-500/10">
        <div class="mx-auto flex h-12 w-12 items-center justify-center rounded-full bg-sky-100 text-sky-700 dark:bg-sky-500/15 dark:text-sky-300">
          <span class="spinner"></span>
        </div>
        <h3 class="mt-4 text-lg font-semibold tracking-tight text-sky-950 dark:text-sky-100">Infrastructure is being torn down</h3>
        <p class="mx-auto mt-2 max-w-2xl text-sm leading-6 text-sky-800/80 dark:text-sky-200/80">
          Cleanup is destroying the Terraform resources and removing local generated output. Cluster details are paused so the panel does not show stale unavailable infrastructure.
        </p>
        <button type="button" data-action="open-cleanup-logs" class="mt-4 rounded-lg border border-sky-200 bg-white px-4 py-2 text-sm font-semibold text-sky-800 shadow-sm hover:bg-sky-50 dark:border-sky-500/30 dark:bg-white/[0.06] dark:text-sky-200 dark:hover:bg-white/[0.1]">Open cleanup logs</button>
      </div>
    `
    return
  }

  const items = clusterItems(state)

  if (!items.length) {
    clustersEl.innerHTML = '<div class="rounded-xl border border-zinc-200 bg-zinc-50 p-4 text-sm text-zinc-600 dark:border-white/10 dark:bg-white/[0.04] dark:text-zinc-400">No clusters discovered yet.</div>'
    return
  }

  clustersEl.innerHTML = items.map(renderCluster).join('')
}

const updateLeaderTracking = state => {
  const messages = []
  const nextLeaders = new Map()

  clusterItems(state).forEach(cluster => {
    const pods = podsFor(cluster)
    const currentLeader = pods.find(pod => pod.leader && pod.leaderLabel === 'Leader') || pods.find(pod => pod.leader)
    const currentLeaderName = currentLeader ? currentLeader.name : ''
    const previousLeaderName = previousLeaders.get(cluster.id) || ''

    if (currentLeaderName) {
      nextLeaders.set(cluster.id, currentLeaderName)
    }

    if (currentLeaderName && previousLeaderName && previousLeaderName !== currentLeaderName) {
      pendingLeaderHighlights.set(cluster.id, currentLeaderName)
      window.setTimeout(() => {
        if (pendingLeaderHighlights.get(cluster.id) === currentLeaderName) {
          pendingLeaderHighlights.delete(cluster.id)
        }
      }, 4500)
      messages.push(`${cluster.name} leader changed to ${currentLeaderName}`)
    }
  })

  previousLeaders = nextLeaders
  lastLeaderChangeMessage = messages.join(' • ')
}

const renderCleanupCost = (cleanup, output) => {
  const cost = parseCleanupCost(output)
  if (cost) {
    cleanupCostEl.classList.remove('hidden')
    cleanupCostEl.innerHTML = `
      <div class="rounded-2xl border border-emerald-200 bg-emerald-50 p-4 text-left dark:border-emerald-500/20 dark:bg-emerald-500/10">
        <div class="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
          <div>
            <div class="text-xs font-semibold uppercase tracking-wide text-emerald-700 dark:text-emerald-300">Estimated infrastructure cost while alive</div>
            <div class="mt-1 text-3xl font-semibold tracking-tight text-emerald-950 dark:text-emerald-100">${escapeHtml(cost.total)}</div>
            <div class="mt-1 text-sm text-emerald-800/80 dark:text-emerald-200/80">${escapeHtml(cost.region || 'AWS region unavailable')}</div>
          </div>
          <div class="grid gap-2 text-sm text-emerald-950 dark:text-emerald-100 sm:min-w-80">
            ${cost.runtime ? `<div><span class="font-semibold">Runtime:</span> ${escapeHtml(cost.runtime)}</div>` : ''}
            ${cost.ec2 ? `<div><span class="font-semibold">EC2:</span> ${escapeHtml(cost.ec2)}</div>` : ''}
            ${cost.ebs ? `<div><span class="font-semibold">EBS:</span> ${escapeHtml(cost.ebs)}</div>` : ''}
          </div>
        </div>
      </div>
    `
    return
  }

  const estimateUnavailable = output.some(line => line.includes('Could not estimate EC2/EBS cost') || line.includes('Terraform outputs unavailable'))
  if (cleanup && cleanup.finishedAt && estimateUnavailable) {
    cleanupCostEl.classList.remove('hidden')
    cleanupCostEl.innerHTML = `
      <div class="rounded-2xl border border-amber-200 bg-amber-50 p-4 text-left text-sm text-amber-800 dark:border-amber-500/20 dark:bg-amber-500/10 dark:text-amber-200">
        Unable to estimate infrastructure cost for this cleanup run. The cleanup still ran; AWS pricing or Terraform outputs were unavailable.
      </div>
    `
    return
  }

  cleanupCostEl.classList.add('hidden')
  cleanupCostEl.innerHTML = ''
}

const renderCleanup = cleanup => {
  const output = cleanup && Array.isArray(cleanup.output) ? cleanup.output : []
  const running = Boolean(cleanup?.running)
  const success = Boolean(cleanup?.finishedAt && !cleanup?.error)
  const failed = Boolean(cleanup?.error)

  if (running) {
    cleanupStatusEl.className = 'inline-flex items-center justify-center rounded-full bg-sky-100 px-3 py-1.5 text-xs font-semibold text-sky-700 dark:bg-sky-500/15 dark:text-sky-300'
    cleanupStatusEl.innerHTML = `<span class="spinner mr-2"></span>Cleanup running${cleanup.startedAt ? ` since ${new Date(cleanup.startedAt).toLocaleTimeString()}` : ''}`
  } else if (failed) {
    cleanupStatusEl.className = 'inline-flex items-center justify-center rounded-full bg-rose-100 px-3 py-1.5 text-xs font-semibold text-rose-700 dark:bg-rose-500/15 dark:text-rose-300'
    cleanupStatusEl.textContent = 'Cleanup finished with error'
  } else if (success) {
    cleanupStatusEl.className = 'inline-flex items-center justify-center rounded-full bg-emerald-100 px-3 py-1.5 text-xs font-semibold text-emerald-700 dark:bg-emerald-500/15 dark:text-emerald-300'
    cleanupStatusEl.textContent = `Cleanup finished successfully at ${new Date(cleanup.finishedAt).toLocaleTimeString()}`
  } else {
    cleanupStatusEl.className = 'inline-flex items-center justify-center rounded-full bg-zinc-100 px-3 py-1.5 text-xs font-semibold text-zinc-600 dark:bg-white/[0.06] dark:text-zinc-300'
    cleanupStatusEl.textContent = 'Idle'
  }

  cleanupActionsEl.className = success
    ? 'mx-auto mt-5 flex max-w-3xl justify-center'
    : 'mx-auto mt-5 grid max-w-3xl gap-3 lg:grid-cols-[minmax(0,1fr)_auto_auto]'

  cleanupConfirmEl.hidden = success
  cleanupBtnEl.hidden = success
  cleanupConfirmEl.disabled = running
  cleanupBtnEl.disabled = running
  cleanupBtnEl.textContent = running ? 'Cleanup running' : 'Run cleanup'
  cleanupBtnEl.className = running
    ? 'rounded-lg bg-zinc-200 px-4 py-2.5 text-sm font-semibold text-zinc-500 shadow-sm dark:bg-white/[0.06] dark:text-zinc-400'
    : 'rounded-lg bg-rose-500 px-4 py-2.5 text-sm font-semibold text-white shadow-sm shadow-rose-500/20 hover:bg-rose-400'

  renderCleanupCost(cleanup, output)

  if (activeLogContext?.mode === 'cleanup') {
    const wasNearBottom = logBoxEl.scrollHeight - logBoxEl.scrollTop - logBoxEl.clientHeight < 80
    rawLogText = output.join('\n')
    setLiveLogState(cleanup?.running ? 'cleanupRunning' : cleanup?.error ? 'cleanupError' : cleanup?.finishedAt ? 'cleanupDone' : 'idle')
    renderLogViewer()
    if (wasNearBottom && !logSearchEl.value.trim()) {
      logBoxEl.scrollTop = logBoxEl.scrollHeight
    }
  }
}

const refresh = async () => {
  if (refreshInFlight) {
    return
  }

  refreshInFlight = true
  refreshStatusEl.textContent = 'Refreshing...'

  try {
    const state = await fetchState()
    lastState = state
    updateLeaderTracking(state)
    renderClusters(state)
    renderCleanup(state.cleanup)
    refreshStatusEl.textContent = lastLeaderChangeMessage
      ? `${lastLeaderChangeMessage} • ${new Date().toLocaleTimeString()}`
      : `Last refreshed at ${new Date().toLocaleTimeString()}`
  } catch (error) {
    refreshStatusEl.textContent = error instanceof Error ? error.message : 'Refresh failed'
  } finally {
    refreshInFlight = false
  }
}

const stopStream = (options = {}) => {
  if (!options.internal && activeLogContext?.mode === 'live' && (liveLogState === 'stopped' || liveLogState === 'error')) {
    if (stream) {
      stream.close()
      stream = null
    }
    streamLogs(activeLogContext.clusterId, activeLogContext.namespace, activeLogContext.podName, { preserveLogs: true })
    return
  }

  if (!stream) {
    return
  }

  stream.close()
  stream = null
  logStatusEl.textContent = 'Live log stream stopped.'
  setLiveLogState('stopped')
  logModalSubtitleEl.textContent = activeLogContext
    ? `${activeLogContext.namespace} • ${activeLogContext.clusterId} • live stream stopped`
    : 'Live log stream stopped.'
}

const loadLogs = async (clusterId, namespace, podName) => {
  stopStream({ internal: true })
  setActiveLogContext('tail', clusterId, namespace, podName)
  setLiveLogState('idle')
  openLogModal()
  rawLogText = ''
  renderLogViewer()
  logStatusEl.textContent = `Loading logs for ${podName}...`

  const params = new URLSearchParams({ cluster: clusterId, namespace, pod: podName })
  const response = await fetch(`/api/logs?${params.toString()}`, {
    headers: {
      'Accept': 'application/json',
      'X-Control-Panel-Token': token
    }
  })
  const raw = await response.text()
  let payload = {}

  try {
    payload = JSON.parse(raw)
  } catch (_) {
    payload = { text: raw }
  }

  if (!response.ok) {
    logStatusEl.textContent = payload.text || 'Failed to load logs'
    rawLogText = payload.text || 'Failed to load logs'
    renderLogViewer()
    return
  }

  rawLogText = payload.text || ''
  renderLogViewer()
  logBoxEl.scrollTop = logBoxEl.scrollHeight
  logStatusEl.textContent = `Showing recent logs for ${podName}`
}

const streamLogs = (clusterId, namespace, podName, options = {}) => {
  stopStream({ internal: true })
  setActiveLogContext('live', clusterId, namespace, podName)
  setLiveLogState('connecting')
  openLogModal()
  if (!options.preserveLogs) {
    rawLogText = ''
  }
  renderLogViewer()
  logStatusEl.textContent = `Streaming live logs for ${podName}...`

  const params = new URLSearchParams({ token, cluster: clusterId, namespace, pod: podName })
  stream = new EventSource(`/api/logs/stream?${params.toString()}`)
  stream.addEventListener('line', event => {
    appendLogLine(event.data)
  })
  stream.addEventListener('error', event => {
    setLiveLogState('error')
    if (event.data) {
      appendLogLine(`[error] ${event.data}`)
    }
  })
  stream.addEventListener('end', () => {
    logStatusEl.textContent = `Live stream finished for ${podName}`
    if (stream) {
      stream.close()
      stream = null
    }
    setLiveLogState('stopped')
  })
}

const downloadKubeconfig = async clusterId => {
  if (activeDownloadClusterId) {
    return
  }

  activeDownloadClusterId = clusterId
  if (lastState) {
    renderClusters(lastState)
  }
  refreshStatusEl.textContent = 'Preparing kubeconfig download...'

  try {
    const response = await fetch(`/api/kubeconfig?cluster=${encodeURIComponent(clusterId)}`, {
      headers: {
        'X-Control-Panel-Token': token
      }
    })

    if (!response.ok) {
      refreshStatusEl.textContent = await response.text()
      return
    }

    const blob = await response.blob()
    const disposition = response.headers.get('Content-Disposition') || ''
    const filenameMatch = disposition.match(/filename="?([^"]+)"?/)
    const filename = filenameMatch ? filenameMatch[1] : 'kubeconfig.yaml'
    const url = URL.createObjectURL(blob)
    const link = document.createElement('a')
    link.href = url
    link.download = filename
    document.body.appendChild(link)
    link.click()
    link.remove()
    URL.revokeObjectURL(url)
    refreshStatusEl.textContent = `Downloaded ${filename}`
  } finally {
    activeDownloadClusterId = ''
    if (lastState) {
      renderClusters(lastState)
    }
  }
}

const copyKubeconfig = async clusterId => {
  if (activeCopyClusterId) {
    return
  }

  if (!navigator.clipboard) {
    refreshStatusEl.textContent = 'Clipboard access is unavailable in this browser.'
    return
  }

  activeCopyClusterId = clusterId
  if (lastState) {
    renderClusters(lastState)
  }
  refreshStatusEl.textContent = 'Copying kubeconfig...'

  try {
    const response = await fetch(`/api/kubeconfig?cluster=${encodeURIComponent(clusterId)}`, {
      headers: {
        'X-Control-Panel-Token': token
      }
    })

    if (!response.ok) {
      refreshStatusEl.textContent = await response.text()
      return
    }

    await navigator.clipboard.writeText(await response.text())
    refreshStatusEl.textContent = 'Copied kubeconfig to clipboard.'
  } catch (error) {
    refreshStatusEl.textContent = error instanceof Error ? error.message : 'Failed to copy kubeconfig.'
  } finally {
    activeCopyClusterId = ''
    if (lastState) {
      renderClusters(lastState)
    }
  }
}

const downloadLogs = () => {
  const text = visibleLogText || rawLogText
  if (!text) {
    logStatusEl.textContent = 'No logs to download yet.'
    return
  }

  const blob = new Blob([text], { type: 'text/plain;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const link = document.createElement('a')
  link.href = url
  link.download = logFilename()
  document.body.appendChild(link)
  link.click()
  link.remove()
  URL.revokeObjectURL(url)
  logStatusEl.textContent = `Downloaded ${link.download}`
}

const openCleanupLogs = () => {
  stopStream({ internal: true })
  setCleanupLogContext()
  const cleanup = lastState?.cleanup || {}
  const output = Array.isArray(cleanup.output) ? cleanup.output : []
  rawLogText = output.join('\n')
  setLiveLogState(cleanup.running ? 'cleanupRunning' : cleanup.error ? 'cleanupError' : cleanup.finishedAt ? 'cleanupDone' : 'idle')
  renderLogViewer()
  openLogModal()
  logBoxEl.scrollTop = logBoxEl.scrollHeight
}

const runCleanup = async () => {
  const confirmValue = cleanupConfirmEl.value.trim()
  if (confirmValue.toLowerCase() !== 'cleanup') {
    cleanupStatusEl.textContent = 'Type cleanup to confirm.'
    return
  }

  const response = await fetch('/api/cleanup', {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      'X-Control-Panel-Token': token
    },
    body: JSON.stringify({ confirm: confirmValue })
  })

  if (!response.ok) {
    cleanupStatusEl.textContent = await response.text()
    return
  }

  cleanupConfirmEl.value = ''
  cleanupStatusEl.textContent = 'Cleanup requested...'
  lastState = {
    ...(lastState || {}),
    cleanup: {
      ...(lastState?.cleanup || {}),
      running: true,
      output: ['[control-panel] Cleanup requested...'],
      startedAt: new Date().toISOString()
    }
  }
  renderClusters(lastState)
  renderCleanup(lastState.cleanup)
  setCleanupLogContext()
  setLiveLogState('cleanupRunning')
  rawLogText = '[control-panel] Cleanup requested...'
  renderLogViewer()
  openLogModal()
  refresh()
}

clustersEl.addEventListener('click', event => {
  const button = event.target.closest('button[data-action]')
  if (!button) {
    return
  }

  const action = button.dataset.action
  const clusterId = button.dataset.cluster

  if (action === 'open-cleanup-logs') {
    openCleanupLogs()
    return
  }

  if (action === 'toggle-cluster') {
    collapsedClusters.set(clusterId, collapsedClusters.get(clusterId) !== true)
    if (lastState) {
      renderClusters(lastState)
    }
    return
  }

  if (action === 'toggle-pods') {
    collapsedPods.set(clusterId, collapsedPods.get(clusterId) !== true)
    if (lastState) {
      renderClusters(lastState)
    }
    return
  }

  if (action === 'download') {
    downloadKubeconfig(clusterId)
    return
  }

  if (action === 'copy-kubeconfig') {
    copyKubeconfig(clusterId)
    return
  }

  if (action === 'tail') {
    loadLogs(clusterId, button.dataset.namespace, button.dataset.pod)
    return
  }

  if (action === 'live') {
    streamLogs(clusterId, button.dataset.namespace, button.dataset.pod)
  }
})

themeToggleEl.addEventListener('click', () => {
  setTheme(currentTheme() === 'dark' ? 'light' : 'dark')
})

document.getElementById('refreshBtn').addEventListener('click', refresh)
document.getElementById('cleanupBtn').addEventListener('click', runCleanup)
openCleanupLogsBtnEl.addEventListener('click', openCleanupLogs)
document.getElementById('stopStreamBtn').addEventListener('click', stopStream)
document.getElementById('clearLogsBtn').addEventListener('click', () => {
  rawLogText = ''
  visibleLogText = ''
  renderLogViewer()
  logStatusEl.textContent = 'Logs cleared.'
})
document.getElementById('downloadLogsBtn').addEventListener('click', downloadLogs)
document.getElementById('closeLogModalBtn').addEventListener('click', closeLogModal)
openLogViewerBtnEl.addEventListener('click', openLogModal)
logSearchEl.addEventListener('input', renderLogViewer)
logLevelFiltersEl.addEventListener('click', event => {
  const button = event.target.closest('button[data-level]')
  if (button) {
    setActiveLogLevel(button.dataset.level)
  }
})

logModalEl.addEventListener('click', event => {
  if (event.target === logModalEl) {
    closeLogModal()
  }
})

document.addEventListener('keydown', event => {
  if (event.key === 'Escape' && !logModalEl.classList.contains('hidden')) {
    closeLogModal()
  }
})

document.body.addEventListener('htmx:afterRequest', event => {
  const requestPath = event.detail.pathInfo?.requestPath || event.detail.xhr?.responseURL || ''
  if (requestPath.includes('/api/shutdown')) {
    window.setTimeout(() => window.close(), 250)
  }
})

setLiveLogState('idle')
setTheme(currentTheme())
refresh()
window.setInterval(refresh, 5000)
