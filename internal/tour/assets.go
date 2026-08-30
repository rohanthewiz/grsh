package tour

import "embed"

// The page is three static files, embedded rather than generated.
//
// grsh's own element library builds HTML from Go, and it is the right tool
// when the markup depends on data. Here it does not: every dynamic part of
// this page — the transcript, the chapter list, the step — is drawn by the
// browser from the SSE stream, so the server's HTML is a fixed skeleton
// with holes in it. Written as Go it would be a worse version of itself,
// with none of an editor's help for the CSS and JavaScript that make up
// nearly all of it.
//
//go:embed assets
var assets embed.FS

//go:embed assets/index.html
var indexHTML []byte
