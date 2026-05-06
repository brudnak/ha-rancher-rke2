tailwind.config = { darkMode: 'class' }

const savedTheme = localStorage.getItem('rancherControlPanelTheme')
const setupTheme = localStorage.getItem('rancherSetupTheme')
const systemDark = window.matchMedia('(prefers-color-scheme: dark)').matches
const theme = savedTheme || setupTheme || (systemDark ? 'dark' : 'light')

document.documentElement.classList.toggle('dark', theme === 'dark')
document.body?.classList.toggle('dark', theme === 'dark')
