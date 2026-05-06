package ui

import _ "embed"

//go:embed templates/interactive_setup.html
var InteractiveSetupHTML string

//go:embed static/interactive_setup.js
var InteractiveSetupJS string

//go:embed templates/control_panel.html
var ControlPanelHTML string

//go:embed static/control_panel.js
var ControlPanelJS string

//go:embed static/control_panel_theme.js
var ControlPanelThemeJS string
