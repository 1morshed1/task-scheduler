package dashboard

import _ "embed"

// indexHTML is the dashboard page, baked into the binary at compile time. Like
// the migrations, embedding means the server ships as one self-contained
// executable with no "did you deploy the static files?" step. The path is
// relative to this source file.
//
//go:embed static/index.html
var indexHTML []byte
