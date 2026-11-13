package chat

import _ "embed"

// Script is the widget sites embed, served at /chat/widget.js.
//
//go:embed widget.js
var Script []byte
