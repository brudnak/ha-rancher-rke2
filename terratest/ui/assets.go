package ui

import _ "embed"

//go:embed templates/interactive_setup.html
var InteractiveSetupHTML string

//go:embed static/interactive_setup.js
var InteractiveSetupJS string
