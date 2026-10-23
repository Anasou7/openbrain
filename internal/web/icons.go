package web

import "html/template"

// icons are 16px line glyphs drawn on a pixel grid, in the spirit of the
// design's compact technical iconography. They inherit currentColor.
var icons = map[string]string{
	"overview": `<rect x="2.5" y="2.5" width="4" height="4"/><rect x="9.5" y="2.5" width="4" height="4"/><rect x="2.5" y="9.5" width="4" height="4"/><rect x="9.5" y="9.5" width="4" height="4"/>`,
	"projects": `<path d="M2.5 4.5h4l1.5 1.5h5.5v7h-11z"/><path d="M2.5 7.5h11"/>`,
	"activity": `<path d="M1.5 8.5h3l2-5 3 9 2-4h3"/>`,
	"agents":   `<rect x="4.5" y="4.5" width="7" height="7"/><rect x="6.5" y="6.5" width="3" height="3"/><path d="M6.5 2v2.5M9.5 2v2.5M6.5 11.5V14M9.5 11.5V14M2 6.5h2.5M2 9.5h2.5M11.5 6.5H14M11.5 9.5H14"/>`,
	"system":   `<path d="M2.5 4.5h11M2.5 8h11M2.5 11.5h11"/><rect x="9.5" y="3" width="2" height="3"/><rect x="4.5" y="6.5" width="2" height="3"/><rect x="8" y="10" width="2" height="3"/>`,
	"account":  `<rect x="5.5" y="2.5" width="5" height="5"/><path d="M2.5 13.5v-1.5a3 3 0 0 1 3-3h5a3 3 0 0 1 3 3v1.5"/>`,
	"signout":  `<path d="M9.5 2.5h-6v11h6M7 8h7M11.5 5.5 14 8l-2.5 2.5"/>`,
	"plus":     `<path d="M8 3v10M3 8h10"/>`,
	"menu":     `<path d="M2.5 4.5h11M2.5 8h11M2.5 11.5h11"/>`,
	"close":    `<path d="M3.5 3.5l9 9M12.5 3.5l-9 9"/>`,
	"collapse": `<path d="M2.5 2.5v11M10.5 4.5 7 8l3.5 3.5M7 8h7"/>`,
	"expand":   `<path d="M13.5 2.5v11M5.5 4.5 9 8l-3.5 3.5M9 8H2"/>`,
	"arrow":    `<path d="M3 8h10M9.5 4.5 13 8l-3.5 3.5"/>`,
	"key":      `<circle cx="5.5" cy="8" r="3"/><path d="M8.5 8h5.5M12 8v2.5M14 8v2"/>`,
}

// icon returns an inline SVG; decorative, so hidden from assistive tech.
func icon(name string) template.HTML {
	return template.HTML(`<svg class="i" viewBox="0 0 16 16" width="16" height="16" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.25" stroke-linecap="square">` + icons[name] + `</svg>`)
}
