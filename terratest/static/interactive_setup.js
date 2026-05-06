const setupData = JSON.parse(document.getElementById('setup-data').textContent || '{}')
const token = setupData.token || ''

let versions = Array.isArray(setupData.versions) ? setupData.versions : ['']
let config = setupData.config || {
  distro: 'auto',
  bootstrapPassword: '',
  preloadImages: false,
  tfVars: {}
}
let customHostnameEnabled = Boolean(setupData.customHostnameEnabled)
let customHostname = ''
let submitting = false
let responseSubmitting = false
let pendingCompletionShouldContinue = true

const rowClass = 'grid gap-3 rounded-xl border border-zinc-200 bg-white p-3 shadow-sm dark:border-white/10 dark:bg-white/[0.03] dark:shadow-none sm:grid-cols-[auto_minmax(0,1fr)_auto] sm:items-center'
const inputClass = 'w-full rounded-lg border border-zinc-200 bg-white px-3.5 py-2.5 font-medium text-zinc-950 outline-none focus:border-emerald-400 dark:border-white/10 dark:bg-zinc-950/50 dark:text-zinc-100'
const removeButtonClass = 'rounded-lg border border-zinc-200 bg-zinc-50 px-3.5 py-2.5 text-sm font-medium text-rose-600 hover:bg-zinc-100 disabled:cursor-default disabled:opacity-60 dark:border-white/10 dark:bg-white/[0.04] dark:text-rose-300 dark:hover:bg-white/[0.08]'
const lockIcon = '<svg xmlns="http://www.w3.org/2000/svg" class="h-4 w-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect width="18" height="11" x="3" y="11" rx="2" ry="2"></rect><path d="M7 11V7a5 5 0 0 1 10 0v4"></path></svg>'
const unlockIcon = '<svg xmlns="http://www.w3.org/2000/svg" class="h-4 w-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect width="18" height="11" x="3" y="11" rx="2" ry="2"></rect><path d="M7 11V7a5 5 0 0 1 9.9-1"></path></svg>'

const setupFormEl = document.getElementById('setupForm')
const rowsEl = document.getElementById('rows')
const totalInstancesValueEl = document.getElementById('totalInstancesValue')
const editorErrorBoxEl = document.getElementById('editorErrorBox')
const editorStatusBoxEl = document.getElementById('editorStatusBox')
const themeToggleEl = document.getElementById('themeToggle')
const themeSunIconEl = document.getElementById('themeSunIcon')
const themeMoonIconEl = document.getElementById('themeMoonIcon')
const themeToggleLabelEl = document.getElementById('themeToggleLabel')
const addBtnEl = document.getElementById('addBtn')
const continueBtnEl = document.getElementById('continueBtn')
const editorCancelBtnEl = document.getElementById('editorCancelBtn')
const customHostnameBoxEl = document.getElementById('customHostnameBox')
const customHostnameToggleEl = document.getElementById('customHostnameToggle')
const customHostnameInputEl = document.getElementById('customHostnameInput')
const distroSelectEl = document.getElementById('distroSelect')
const bootstrapPasswordInputEl = document.getElementById('bootstrapPasswordInput')
const bootstrapPasswordToggleEl = document.getElementById('bootstrapPasswordToggle')
const preloadImagesToggleEl = document.getElementById('preloadImagesToggle')
const tfVarInputEls = Array.from(document.querySelectorAll('input[data-tf-var]'))
const lockedFieldInputEls = Array.from(document.querySelectorAll('input[data-locked-field]'))
const lockToggleEls = Array.from(document.querySelectorAll('button[data-lock-toggle]'))
const secretToggleEls = Array.from(document.querySelectorAll('button[data-secret-toggle]'))
const logPanelEl = document.getElementById('logPanel')
const resolvingErrorBoxEl = document.getElementById('resolvingErrorBox')
const planPanelEl = document.getElementById('planPanel')
const reviewErrorBoxEl = document.getElementById('reviewErrorBox')
const respondActionsEl = document.getElementById('respondActions')
const doneAccentEl = document.getElementById('doneAccent')
const doneIconEl = document.getElementById('doneIcon')
const doneTitleEl = document.getElementById('doneTitle')
const doneBodyEl = document.getElementById('doneBody')
const doneDetailEl = document.getElementById('doneDetail')
const confirmModalEl = document.getElementById('confirmModal')
const confirmModalTitleEl = document.getElementById('confirmModalTitle')
const confirmModalBodyEl = document.getElementById('confirmModalBody')
const confirmModalConfirmEl = document.getElementById('confirmModalConfirm')
const confirmModalCancelEl = document.getElementById('confirmModalCancel')

const setPhase = phase => {
  if (phase === 'done') {
    renderCompletion(pendingCompletionShouldContinue)
  }

  document.body.dataset.phase = phase
}

const currentTheme = () => document.documentElement.classList.contains('dark') ? 'dark' : 'light'

const persistTheme = theme => {
  localStorage.setItem('rancherSetupTheme', theme)
  document.cookie = `rancherSetupTheme=${theme}; Path=/; Max-Age=31536000; SameSite=Lax`
}

const setTheme = (theme, persist = true) => {
  document.documentElement.classList.toggle('dark', theme === 'dark')

  if (persist) {
    persistTheme(theme)
  }

  if (themeToggleLabelEl && themeSunIconEl && themeMoonIconEl) {
    themeSunIconEl.classList.toggle('hidden', theme !== 'dark')
    themeMoonIconEl.classList.toggle('hidden', theme !== 'light')
    themeToggleLabelEl.textContent = theme === 'dark' ? 'Light' : 'Dark'
  }

}

const escapeHtml = value => String(value)
  .replaceAll('&', '&amp;')
  .replaceAll('<', '&lt;')
  .replaceAll('>', '&gt;')
  .replaceAll('"', '&quot;')

const sanitizeDisplayValue = value => {
  let next = String(value || '').trim()

  while (next.length >= 2) {
    const first = next[0]
    const last = next[next.length - 1]

    if ((first === '"' && last === '"') || (first === '\'' && last === '\'')) {
      next = next.slice(1, -1).trim()
      continue
    }

    break
  }

  return next
}

customHostname = sanitizeDisplayValue(setupData.customHostname || '')

const showValidationError = (message, target) => {
  editorErrorBoxEl.textContent = message
  editorStatusBoxEl.textContent = ''
  editorErrorBoxEl.scrollIntoView({ behavior: 'smooth', block: 'center' })

  if (target) {
    target.focus({ preventScroll: true })
  }
}

const clearValidationError = () => {
  editorErrorBoxEl.textContent = ''
}

const showConfirmModal = ({ title, body, confirmText = 'Continue', cancelText = 'Go back', showCancel = true }) => new Promise(resolve => {
  if (!confirmModalEl || !confirmModalTitleEl || !confirmModalBodyEl || !confirmModalConfirmEl || !confirmModalCancelEl) {
    resolve(true)
    return
  }

  let settled = false

  const settle = result => {
    if (settled) {
      return
    }

    settled = true
    confirmModalEl.classList.add('hidden')
    confirmModalEl.classList.remove('flex')
    confirmModalConfirmEl.removeEventListener('click', confirm)
    confirmModalCancelEl.removeEventListener('click', cancel)
    confirmModalEl.removeEventListener('click', backdropCancel)
    document.removeEventListener('keydown', escapeCancel)
    resolve(result)
  }

  const confirm = () => settle(true)
  const cancel = () => settle(false)
  const backdropCancel = event => {
    if (event.target === confirmModalEl) {
      cancel()
    }
  }
  const escapeCancel = event => {
    if (event.key === 'Escape') {
      cancel()
    }
  }

  confirmModalTitleEl.textContent = title
  confirmModalBodyEl.textContent = body
  confirmModalConfirmEl.textContent = confirmText
  confirmModalCancelEl.textContent = cancelText
  confirmModalCancelEl.classList.toggle('hidden', !showCancel)
  confirmModalEl.classList.remove('hidden')
  confirmModalEl.classList.add('flex')
  confirmModalConfirmEl.addEventListener('click', confirm)
  confirmModalCancelEl.addEventListener('click', cancel)
  confirmModalEl.addEventListener('click', backdropCancel)
  document.addEventListener('keydown', escapeCancel)
  confirmModalConfirmEl.focus()
})

const showNoticeModal = ({ title, body, confirmText = 'Got it' }) => showConfirmModal({
  title,
  body,
  confirmText,
  showCancel: false
})

const renderEditableConfig = () => {
  distroSelectEl.value = config.distro || 'auto'
  bootstrapPasswordInputEl.value = config.bootstrapPassword || ''
  preloadImagesToggleEl.checked = Boolean(config.preloadImages)

  tfVarInputEls.forEach(input => {
    const key = input.getAttribute('data-tf-var')
    input.value = (config.tfVars && config.tfVars[key]) || ''
  })

  lockAllAdvancedAWSFields()
}

const renderRows = () => {
  if (customHostnameEnabled && versions.length !== 1) {
    versions = [versions[0] || '']
  }

  rowsEl.innerHTML = versions.map((version, index) => {
    const removeDisabled = customHostnameEnabled || versions.length <= 1 ? ' disabled' : ''

    return [
      `<div class="${rowClass}">`,
      `<div class="inline-flex w-fit rounded-md bg-zinc-100 px-2.5 py-1 text-sm font-medium text-zinc-600 dark:bg-white/[0.06] dark:text-zinc-300">HA ${index + 1}</div>`,
      `<div><input class="${inputClass}" type="text" name="versions" value="${escapeHtml(version)}" data-index="${index}" placeholder="2.14.1-alpha3" /></div>`,
      `<div><button class="${removeButtonClass}" type="button" data-remove-index="${index}"${removeDisabled}>Remove</button></div>`,
      '</div>'
    ].join('')
  }).join('')

  totalInstancesValueEl.textContent = String(versions.length)
  addBtnEl.disabled = submitting
  addBtnEl.setAttribute('aria-disabled', customHostnameEnabled ? 'true' : 'false')
  addBtnEl.classList.toggle('cursor-not-allowed', customHostnameEnabled)
  addBtnEl.classList.toggle('opacity-50', customHostnameEnabled)

  rowsEl.querySelectorAll('input[data-index]').forEach(input => {
    input.addEventListener('input', event => {
      versions[Number(event.target.getAttribute('data-index'))] = event.target.value
      clearValidationError()
    })
  })

  rowsEl.querySelectorAll('button[data-remove-index]').forEach(button => {
    button.addEventListener('click', () => {
      if (versions.length <= 1 || submitting || customHostnameEnabled) {
        return
      }

      versions.splice(Number(button.getAttribute('data-remove-index')), 1)
      renderRows()
    })
  })
}

const renderCustomHostname = () => {
  customHostnameBoxEl.dataset.enabled = customHostnameEnabled ? 'true' : 'false'
  customHostnameToggleEl.checked = customHostnameEnabled
  customHostnameInputEl.value = customHostname
  renderRows()
}

const normalizeVersion = value => String(value || '').trim().replace(/^[vV]/, '')

const normalizedVersions = () => versions.map(version => normalizeVersion(version))

const normalizedAWSPrefix = () => {
  const input = document.querySelector('input[data-tf-var="aws_prefix"]')
  return String((input && input.value) || '').trim().toLowerCase()
}

const collectTFVars = () => {
  const tfVars = {}

  tfVarInputEls.forEach(input => {
    const key = input.getAttribute('data-tf-var')
    tfVars[key] = String(input.value || '').trim()
  })

  tfVars.aws_prefix = normalizedAWSPrefix()

  const prefixInput = document.querySelector('input[data-tf-var="aws_prefix"]')
  if (prefixInput) {
    prefixInput.value = tfVars.aws_prefix
  }

  return tfVars
}

const validateSetup = () => {
  const trimmed = normalizedVersions()

  if (trimmed.length < 1) {
    return { message: 'At least one HA version is required.', target: rowsEl.querySelector('input[data-index]') }
  }

  for (let i = 0; i < trimmed.length; i += 1) {
    if (!trimmed[i]) {
      return {
        message: `Version for HA ${i + 1} cannot be empty.`,
        target: rowsEl.querySelector(`input[data-index="${i}"]`)
      }
    }
  }

  if (customHostnameEnabled) {
    if (trimmed.length !== 1) {
      return { message: 'Custom Rancher URL can only be used with one HA.', target: customHostnameToggleEl }
    }

    if (!String(customHostname || '').trim()) {
      return { message: 'Enter a custom Rancher URL label.', target: customHostnameInputEl }
    }
  }

  const prefixInput = document.querySelector('input[data-tf-var="aws_prefix"]')
  const prefix = normalizedAWSPrefix()

  if (!/^[a-z]{2,3}$/.test(prefix)) {
    return {
      message: 'AWS prefix must be 2 or 3 letters, usually your initials.',
      target: prefixInput
    }
  }

  const pemKeyInput = document.querySelector('input[data-tf-var="aws_pem_key_name"]')

  if (!String((pemKeyInput && pemKeyInput.value) || '').trim()) {
    return {
      message: 'AWS PEM key name is required.',
      target: pemKeyInput,
      notice: true
    }
  }

  if (!bootstrapPasswordInputEl.value.trim()) {
    return {
      message: 'Bootstrap password cannot be empty.',
      target: bootstrapPasswordInputEl
    }
  }

  return null
}

const setFieldLocked = (key, locked) => {
  const input = document.querySelector(`input[data-tf-var="${key}"]`)
  const button = document.querySelector(`button[data-lock-toggle="${key}"]`)

  if (!input || !button) {
    return
  }

  input.readOnly = locked
  button.innerHTML = locked ? lockIcon : unlockIcon
  button.dataset.state = locked ? 'locked' : 'unlocked'
  button.title = `${locked ? 'Unlock' : 'Lock'} ${input.closest('label')?.firstChild?.textContent.trim() || 'field'}`
  button.setAttribute('aria-label', button.title)

  button.classList.toggle('text-emerald-600', !locked)
  button.classList.toggle('dark:text-emerald-400', !locked)
  button.classList.toggle('text-zinc-500', locked)
  button.classList.toggle('dark:text-zinc-400', locked)
  input.classList.toggle('text-zinc-950', !locked)
  input.classList.toggle('dark:text-zinc-100', !locked)
  input.classList.toggle('text-zinc-500', locked)
  input.classList.toggle('dark:text-zinc-500', locked)
  input.classList.toggle('bg-white', !locked)
  input.classList.toggle('dark:bg-zinc-950/50', !locked)
  input.classList.toggle('bg-zinc-100', locked)
  input.classList.toggle('dark:bg-zinc-950/30', locked)
}

const lockAllAdvancedAWSFields = () => {
  lockedFieldInputEls.forEach(input => {
    setFieldLocked(input.getAttribute('data-tf-var'), true)
  })
}

const toggleBootstrapPasswordVisibility = () => {
  const showing = bootstrapPasswordInputEl.type === 'text'
  bootstrapPasswordInputEl.type = showing ? 'password' : 'text'
  bootstrapPasswordToggleEl.textContent = showing ? 'Show' : 'Hide'
}

const toggleSecretFieldVisibility = key => {
  const input = document.querySelector(`input[data-tf-var="${key}"]`)
  const button = document.querySelector(`button[data-secret-toggle="${key}"]`)

  if (!input || !button) {
    return
  }

  const showing = input.type === 'text'
  input.type = showing ? 'password' : 'text'
  button.textContent = showing ? 'Show' : 'Hide'
}

const completionCopy = shouldContinue => shouldContinue
  ? {
      title: 'Response recorded',
      body: 'You can close this tab. The test run is continuing in your terminal.',
      detail: 'Setup approval has been handed back to the local run.',
      accentClass: 'flex h-11 w-11 items-center justify-center rounded-full bg-emerald-100 text-emerald-700 dark:bg-emerald-500/15 dark:text-emerald-300',
      icon: '<path d="M20 6 9 17l-5-5"></path>'
    }
  : {
      title: 'Setup canceled',
      body: 'You can close this tab. The local test run will stop with a canceled setup message.',
      detail: 'No Rancher HA plan was approved from this browser session.',
      accentClass: 'flex h-11 w-11 items-center justify-center rounded-full bg-rose-100 text-rose-700 dark:bg-rose-500/15 dark:text-rose-300',
      icon: '<path d="M18 6 6 18"></path><path d="m6 6 12 12"></path>'
    }

const renderCompletion = shouldContinue => {
  const copy = completionCopy(shouldContinue)

  doneAccentEl.className = copy.accentClass
  doneIconEl.innerHTML = copy.icon
  doneTitleEl.textContent = copy.title
  doneBodyEl.textContent = copy.body
  doneDetailEl.textContent = copy.detail
}

const setSubmittingState = nextSubmitting => {
  submitting = nextSubmitting
  addBtnEl.disabled = nextSubmitting
  continueBtnEl.disabled = nextSubmitting
  editorCancelBtnEl.disabled = nextSubmitting
  customHostnameToggleEl.disabled = nextSubmitting
  customHostnameInputEl.disabled = nextSubmitting
  distroSelectEl.disabled = nextSubmitting
  bootstrapPasswordInputEl.disabled = nextSubmitting
  bootstrapPasswordToggleEl.disabled = nextSubmitting
  preloadImagesToggleEl.disabled = nextSubmitting

  tfVarInputEls.forEach(input => {
    input.disabled = nextSubmitting
  })

  lockToggleEls.forEach(button => {
    button.disabled = nextSubmitting
  })

  secretToggleEls.forEach(button => {
    button.disabled = nextSubmitting
  })

  rowsEl.querySelectorAll('input, button[data-remove-index]').forEach(element => {
    element.disabled = nextSubmitting ||
      (element.hasAttribute('data-remove-index') && (customHostnameEnabled || versions.length <= 1))
  })
}

const prepareSetupSubmit = async event => {
  if (event) {
    event.preventDefault()
    event.stopPropagation()
  }

  if (submitting) {
    return
  }

  const validationError = validateSetup()

  if (validationError) {
    showValidationError(validationError.message, validationError.target)
    if (validationError.notice) {
      await showNoticeModal({
        title: 'PEM key name required',
        body: 'Add the AWS PEM key name before resolving the plan. It should match the EC2 key pair name for your AWS account.'
      })
    }
    return
  }

  clearValidationError()

  const tfVars = collectTFVars()

  const prefixConfirmed = await showConfirmModal({
    title: 'Confirm AWS prefix',
    body: `AWS prefix is "${tfVars.aws_prefix}". This should be your initials and will be used to label AWS resources.`,
    confirmText: 'Use this prefix'
  })

  if (!prefixConfirmed) {
    return
  }

  const pemConfirmed = await showConfirmModal({
    title: 'Confirm PEM key name',
    body: `AWS PEM key name is "${tfVars.aws_pem_key_name}". This must match the EC2 key pair you want the run to use.`,
    confirmText: 'Use this key'
  })

  if (!pemConfirmed) {
    return
  }

  editorStatusBoxEl.textContent = 'Saving config and kicking off plan resolution...'

  if (window.htmx) {
    window.htmx.trigger(setupFormEl, 'confirmed-submit')
  } else {
    setupFormEl.submit()
  }

  window.setTimeout(() => setSubmittingState(true), 0)
}

const cancelEditor = () => {
  if (submitting) {
    return
  }

  sendResponse('cancel')
}

const responseErrorBox = () => document.body.dataset.phase === 'review' ? reviewErrorBoxEl : editorErrorBoxEl

const setResponseButtonsDisabled = disabled => {
  if (!respondActionsEl) {
    return
  }

  respondActionsEl.querySelectorAll('button[data-response-action]').forEach(button => {
    button.disabled = disabled
  })
}

const sendResponse = async action => {
  if (responseSubmitting) {
    return
  }

  responseSubmitting = true
  const shouldContinue = action === 'continue'
  pendingCompletionShouldContinue = shouldContinue
  const body = new URLSearchParams()
  body.set('token', token)
  body.set('action', action)

  setResponseButtonsDisabled(true)
  reviewErrorBoxEl.textContent = ''
  editorErrorBoxEl.textContent = ''

  try {
    const response = await fetch(`/respond?token=${encodeURIComponent(token)}`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/x-www-form-urlencoded'
      },
      body
    })

    if (!response.ok) {
      responseErrorBox().textContent = await response.text()
      responseSubmitting = false
      setResponseButtonsDisabled(false)
      return
    }

    renderCompletion(shouldContinue)
    setPhase('done')
  } catch (error) {
    responseErrorBox().textContent = error instanceof Error ? error.message : 'Failed to send setup response.'
    responseSubmitting = false
    setResponseButtonsDisabled(false)
  }
}

const appendLogLine = line => {
  const empty = logPanelEl.querySelector('span')

  if (empty && empty.textContent.includes('Waiting for resolver output')) {
    empty.remove()
  }

  const span = document.createElement('span')
  span.className = 'block'
  span.textContent = line
  logPanelEl.appendChild(span)
  logPanelEl.scrollTop = logPanelEl.scrollHeight
}

const connectEventStream = () => {
  const source = new EventSource(`/events?token=${encodeURIComponent(token)}`)

  source.onmessage = event => {
    let payload

    try {
      payload = JSON.parse(event.data)
    } catch (_) {
      return
    }

    switch (payload.type) {
      case 'phase':
        setPhase(payload.phase)
        if (payload.phase === 'done') {
          source.close()
        }
        break
      case 'log':
        appendLogLine(payload.line)
        break
      case 'plan':
        planPanelEl.textContent = payload.plan
        break
      case 'error':
        resolvingErrorBoxEl.textContent = payload.error
        reviewErrorBoxEl.textContent = payload.error
        break
    }
  }

  source.onerror = () => {}
}

addBtnEl.addEventListener('click', () => {
  if (submitting) {
    return
  }

  if (customHostnameEnabled) {
    showNoticeModal({
      title: 'Custom URL is limited to one HA',
      body: 'A custom Rancher URL creates exactly one HA because the DNS name must be unique. Turn off "Use a custom Rancher URL" if you want to add more than one HA.',
      confirmText: 'Got it'
    })
    return
  }

  versions.push('')
  renderRows()
})

bootstrapPasswordToggleEl.addEventListener('click', toggleBootstrapPasswordVisibility)

themeToggleEl.addEventListener('click', () => {
  setTheme(currentTheme() === 'dark' ? 'light' : 'dark')
})

lockToggleEls.forEach(button => {
  button.addEventListener('mousedown', event => {
    event.preventDefault()
  })

  button.addEventListener('click', () => {
    if (submitting) {
      return
    }

    const key = button.getAttribute('data-lock-toggle')
    const input = document.querySelector(`input[data-tf-var="${key}"]`)

    if (!input) {
      return
    }

    const willUnlock = input.readOnly
    setFieldLocked(key, !willUnlock)

    if (willUnlock) {
      input.focus()
      input.select()
    }
  })
})

lockedFieldInputEls.forEach(input => {
  input.addEventListener('blur', () => {
    if (submitting) {
      return
    }

    setFieldLocked(input.getAttribute('data-tf-var'), true)
  })
})

secretToggleEls.forEach(button => {
  button.addEventListener('click', () => {
    if (submitting) {
      return
    }

    toggleSecretFieldVisibility(button.getAttribute('data-secret-toggle'))
  })
})

customHostnameToggleEl.addEventListener('change', event => {
  if (submitting) {
    return
  }

  customHostnameEnabled = event.target.checked
  clearValidationError()
  renderCustomHostname()
})

customHostnameInputEl.addEventListener('input', event => {
  customHostname = event.target.value
  clearValidationError()
})

editorCancelBtnEl.addEventListener('click', cancelEditor)
setupFormEl.addEventListener('submit', prepareSetupSubmit)
continueBtnEl.addEventListener('click', prepareSetupSubmit)

if (respondActionsEl) {
  respondActionsEl.querySelectorAll('button[data-response-action]').forEach(button => {
    button.addEventListener('click', () => {
      sendResponse(button.getAttribute('data-response-action'))
    })
  })
}

document.body.addEventListener('htmx:afterRequest', event => {
  const requestEl = event.detail.elt

  if (requestEl !== setupFormEl && !setupFormEl.contains(requestEl)) {
    return
  }

  if (event.detail.successful) {
    return
  }

  showValidationError(event.detail.xhr.responseText || 'Setup submit failed.')
  setSubmittingState(false)
})

renderCustomHostname()
renderEditableConfig()
setTheme(currentTheme(), false)
connectEventStream()
