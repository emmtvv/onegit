package web

import (
	"fmt"
	"html/template"
	"path"
	"strings"
	"unicode/utf8"
)

// File icons in the spirit of editor icon themes: every kind of file gets its
// own silhouette (a language mark, a note for audio, a film frame for video, …)
// in its own colour. Symbols are 16×16 and drawn with currentColor; the colour
// comes from the --fc custom property, white is used for knock-outs.

type fileKind struct {
	icon  string // a key of symbols, or a labelled shape (badge, circle, hex, shield, ellipse, page, corner)
	color string
	label string // text for labelled shapes
	ink   string // label colour, white by default
}

func sym(icon, color string) fileKind { return fileKind{icon: icon, color: color} }
func lbl(shape, color, label string) fileKind {
	return fileKind{icon: shape, color: color, label: label}
}
func ink(shape, color, label, c string) fileKind {
	return fileKind{icon: shape, color: color, label: label, ink: c}
}

// symbols are complete icons.
var symbols = map[string]string{
	"plain":    `<path d="M4.25 1.5h5.1l3.4 3.4v8.35c0 .69-.56 1.25-1.25 1.25h-7.25C3.56 14.5 3 13.94 3 13.25v-10.5C3 2.06 3.56 1.5 4.25 1.5Z" fill="none" stroke="currentColor" stroke-width="1.1" stroke-linejoin="round"/><path d="M9.2 1.6v2.6c0 .5.4.9.9.9h2.6" fill="none" stroke="currentColor" stroke-width="1.1" stroke-linejoin="round"/>`,
	"lines":    `<path d="M4.25 1.5h5.1l3.4 3.4v8.35c0 .69-.56 1.25-1.25 1.25h-7.25C3.56 14.5 3 13.94 3 13.25v-10.5C3 2.06 3.56 1.5 4.25 1.5Z" fill="currentColor" fill-opacity=".14" stroke="currentColor" stroke-width="1.1" stroke-linejoin="round"/><path d="M5.3 7.3h5.4M5.3 9.5h5.4M5.3 11.7h3.6" stroke="currentColor" stroke-width="1.1" stroke-linecap="round"/>`,
	"note":     `<path d="M6 12V3.3l7.5-1.8V10.3" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linejoin="round"/><ellipse cx="4" cy="12.2" rx="2.4" ry="1.9" fill="currentColor"/><ellipse cx="11.5" cy="10.5" rx="2.4" ry="1.9" fill="currentColor"/><path d="M6 5.4l7.5-1.8" stroke="currentColor" stroke-width="1.6"/>`,
	"film":     `<rect x="1" y="2.5" width="14" height="11" rx="2.2" fill="currentColor"/><path d="M6.4 5.5v5L10.7 8Z" fill="#fff"/><path d="M2.2 4h1.1v1.1H2.2zM2.2 7.45h1.1v1.1H2.2zM2.2 10.9h1.1V12H2.2zM12.7 4h1.1v1.1h-1.1zM12.7 7.45h1.1v1.1h-1.1zM12.7 10.9h1.1V12h-1.1z" fill="#fff" fill-opacity=".75"/>`,
	"picture":  `<rect x="1" y="2" width="14" height="12" rx="2.5" fill="currentColor"/><circle cx="10.9" cy="5.6" r="1.5" fill="#fff"/><path d="M2.4 12.6 6.1 7.9l2.7 3.2 1.7-1.9 3.1 3.4Z" fill="#fff"/>`,
	"vector":   `<path d="M3 13C3.6 6.3 12.4 9.7 13 3" fill="none" stroke="currentColor" stroke-width="1.6"/><path d="M3 13h6M13 3H7" stroke="currentColor" stroke-width=".9" stroke-dasharray="1.2 1"/><rect x="1.2" y="11.2" width="3.6" height="3.6" rx=".7" fill="currentColor"/><rect x="11.2" y="1.2" width="3.6" height="3.6" rx=".7" fill="currentColor"/><circle cx="9" cy="13" r="1.1" fill="currentColor"/><circle cx="7" cy="3" r="1.1" fill="currentColor"/>`,
	"braces":   `<path d="M5.5 2C3.9 2 3.7 3 3.7 4.2V6C3.7 7.2 3.1 8 2 8c1.1 0 1.7.8 1.7 2v1.8C3.7 13 3.9 14 5.5 14M10.5 2c1.6 0 1.8 1 1.8 2.2V6c0 1.2.6 2 1.7 2-1.1 0-1.7.8-1.7 2v1.8c0 1.2-.2 2.2-1.8 2.2" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"/><circle cx="8" cy="8" r="1.2" fill="currentColor"/>`,
	"bars":     `<rect x="1.5" y="2" width="9" height="2.6" rx="1.3" fill="currentColor"/><rect x="4.5" y="6.7" width="10" height="2.6" rx="1.3" fill="currentColor"/><rect x="4.5" y="11.4" width="7" height="2.6" rx="1.3" fill="currentColor"/>`,
	"brackets": `<path d="M5 1.8H2.3v12.4H5M11 1.8h2.7v12.4H11" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"/><path d="M5.5 5.6h5M5.5 8h5M5.5 10.4h3" stroke="currentColor" stroke-width="1.4" stroke-linecap="round"/>`,
	"sliders":  `<path d="M2 4.5h12M2 11.5h12" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/><circle cx="10" cy="4.5" r="2.4" fill="currentColor"/><circle cx="10" cy="4.5" r="1" fill="#fff"/><circle cx="6" cy="11.5" r="2.4" fill="currentColor"/><circle cx="6" cy="11.5" r="1" fill="#fff"/>`,
	"angle":    `<path d="M5 4 1.5 8 5 12M11 4l3.5 4-3.5 4M9.4 2.5 6.6 13.5" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/>`,
	"sheet":    `<rect x="1.5" y="1.5" width="13" height="13" rx="2.2" fill="currentColor"/><path d="M1.5 5.8h13M1.5 10.2h13M6 5.8v8.7" stroke="#fff" stroke-width="1"/>`,
	"cylinder": `<ellipse cx="8" cy="3.6" rx="6" ry="2.2" fill="currentColor"/><path d="M2 3.6v8.8c0 1.2 2.7 2.2 6 2.2s6-1 6-2.2V3.6c0 1.2-2.7 2.2-6 2.2S2 4.8 2 3.6Z" fill="currentColor" fill-opacity=".78"/><path d="M2 8c0 1.2 2.7 2.2 6 2.2s6-1 6-2.2" fill="none" stroke="#fff" stroke-opacity=".7"/>`,
	"terminal": `<rect x="1" y="2" width="14" height="12" rx="2.5" fill="currentColor"/><path d="m4 5.8 2.4 2.2L4 10.2M8 10.6h4" fill="none" stroke="#fff" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round"/>`,
	"whale":    `<path d="M1 8.2h11.7c.5-1.3 1.6-1.9 2.8-1.6-.4 1.3-1 1.9-1.8 2.1C12.8 12 10.4 14 6.6 14 3.2 14 1.4 11.8 1 8.2Z" fill="currentColor"/><path d="M2.6 5.7h2v2h-2zM4.9 5.7h2v2h-2zM7.2 5.7h2v2h-2zM4.9 3.4h2v2h-2zM7.2 3.4h2v2h-2zM7.2 1.1h2v2h-2z" fill="currentColor"/><circle cx="4.3" cy="10.4" r=".7" fill="#fff"/>`,
	"gear":     `<circle cx="8" cy="8" r="5.1" fill="none" stroke="currentColor" stroke-width="3.2" stroke-dasharray="2.2 1.8"/><circle cx="8" cy="8" r="4.4" fill="currentColor"/><circle cx="8" cy="8" r="1.7" fill="#fff"/>`,
	"git":      `<rect x="2.6" y="2.6" width="10.8" height="10.8" rx="1.8" transform="rotate(45 8 8)" fill="currentColor"/><path d="M6.6 4.6 8 6v5.2M8 6l2.4 2.4" fill="none" stroke="#fff" stroke-width="1.1"/><circle cx="8" cy="6" r="1.05" fill="#fff"/><circle cx="8" cy="11.2" r="1.05" fill="#fff"/><circle cx="10.4" cy="8.4" r="1.05" fill="#fff"/>`,
	"padlock":  `<rect x="2.5" y="7" width="11" height="8" rx="2" fill="currentColor"/><path d="M5 7V5a3 3 0 0 1 6 0v2" fill="none" stroke="currentColor" stroke-width="1.7"/><circle cx="8" cy="10.6" r="1.2" fill="#fff"/><path d="M8 11v1.8" stroke="#fff" stroke-width="1"/>`,
	"key":      `<circle cx="5" cy="8" r="3.9" fill="currentColor"/><circle cx="4.1" cy="8" r="1.2" fill="#fff"/><path d="M8.4 8H15M12.6 8v2.7M14.7 8v1.9" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"/>`,
	"ribbon":   `<circle cx="8" cy="6" r="4.9" fill="currentColor"/><path d="m5.2 9.8-1 5.2L8 13l3.8 2-1-5.2" fill="currentColor"/><circle cx="8" cy="6" r="2.4" fill="none" stroke="#fff" stroke-width="1"/>`,
	"book":     `<path d="M1.3 2.8c2.3-.9 4.7-.8 6.4.7v10.9c-1.8-1.4-4.1-1.6-6.4-.7Z" fill="currentColor"/><path d="M14.7 2.8c-2.3-.9-4.7-.8-6.4.7v10.9c1.8-1.4 4.1-1.6 6.4-.7Z" fill="currentColor" fill-opacity=".72"/>`,
	"people":   `<circle cx="5.5" cy="5" r="2.5" fill="currentColor"/><circle cx="11.2" cy="5.7" r="2" fill="currentColor" fill-opacity=".75"/><path d="M.9 13.8c.3-3 2.1-4.7 4.6-4.7s4.3 1.7 4.6 4.7Z" fill="currentColor"/><path d="M10.8 13.8c-.1-1.6-.6-2.9-1.4-3.9.5-.3 1.1-.5 1.8-.5 2.1 0 3.6 1.5 4 4.4Z" fill="currentColor" fill-opacity=".75"/>`,
	"box":      `<rect x="2" y="5.4" width="12" height="9.1" rx="1.5" fill="currentColor"/><rect x="1.2" y="1.8" width="13.6" height="3.9" rx="1.2" fill="currentColor" fill-opacity=".72"/><path d="M8 1.8V4M8 6.4v1.3M8 8.9v1.3" stroke="#fff" stroke-width="1.2"/><rect x="6.8" y="10.8" width="2.4" height="2.4" rx=".5" fill="#fff"/>`,
	"font":     `<text x="8" y="12.6" font-size="11" font-weight="700" font-family="Georgia,'Times New Roman',serif" text-anchor="middle" textLength="15" lengthAdjust="spacingAndGlyphs" fill="currentColor">Aa</text>`,
	"cube":     `<path d="M8 .9 14.5 4.5v7L8 15.1 1.5 11.5v-7Z" fill="currentColor"/><path d="M1.8 4.6 8 8l6.2-3.4M8 8v6.9" fill="none" stroke="#fff" stroke-opacity=".6" stroke-width="1"/>`,
	"flask":    `<path d="M5.8 1.5h4.4M6.6 1.6v4.2L2.3 12.6c-.6 1 .1 1.9 1.1 1.9h9.2c1 0 1.7-.9 1.1-1.9L9.4 5.8V1.6" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linejoin="round" stroke-linecap="round"/><path d="M4.5 10h7l1.7 2.6c.3.5 0 1-.6 1H3.4c-.6 0-.9-.5-.6-1Z" fill="currentColor"/>`,
	"chip":     `<rect x="3.5" y="3.5" width="9" height="9" rx="1.6" fill="currentColor"/><path d="M6 1v2.5M10 1v2.5M6 12.5V15M10 12.5V15M1 6h2.5M1 10h2.5M12.5 6H15M12.5 10H15" stroke="currentColor" stroke-width="1.3"/><rect x="6" y="6" width="4" height="4" rx=".6" fill="#fff" fill-opacity=".65"/>`,
	"disc":     `<circle cx="8" cy="8" r="7" fill="currentColor"/><circle cx="8" cy="8" r="2" fill="#fff"/><path d="M3.6 5.6a4.8 4.8 0 0 1 2.3-2.1M12.4 10.4a4.8 4.8 0 0 1-2.3 2.1" fill="none" stroke="#fff" stroke-opacity=".6" stroke-width="1"/>`,
	"md":       `<rect x=".8" y="3.1" width="14.4" height="9.8" rx="2" fill="currentColor"/><path d="M3.2 10.4V5.6l2.1 2.5 2.1-2.5v4.8M11.3 5.6v4.5m-1.9-1.9 1.9 2 1.9-2" fill="none" stroke="#fff" stroke-width="1.3" stroke-linecap="round" stroke-linejoin="round"/>`,
	// language marks
	"python":    `<path d="M7.9 1C5.6 1 5 2 5 3.2V5h3.2v.7H3.4C2.1 5.7 1 6.6 1 8.4c0 1.9.9 3 2.3 3h1.4V9.6c0-1.3 1.1-2.3 2.4-2.3h3c1 0 1.9-.9 1.9-1.9V3.2C12 2 11 1 7.9 1Z" fill="#3776ab"/><circle cx="6.5" cy="2.8" r=".65" fill="#fff"/><g transform="rotate(180 8 8)"><path d="M7.9 1C5.6 1 5 2 5 3.2V5h3.2v.7H3.4C2.1 5.7 1 6.6 1 8.4c0 1.9.9 3 2.3 3h1.4V9.6c0-1.3 1.1-2.3 2.4-2.3h3c1 0 1.9-.9 1.9-1.9V3.2C12 2 11 1 7.9 1Z" fill="#f2c230"/><circle cx="6.5" cy="2.8" r=".65" fill="#fff"/></g>`,
	"rust":      `<circle cx="8" cy="8" r="6.1" fill="none" stroke="currentColor" stroke-width="2.2" stroke-dasharray="1.35 1.1"/><circle cx="8" cy="8" r="5.1" fill="currentColor"/><text x="8" y="10.9" font-size="8" font-weight="700" font-family="Onest,Arial,sans-serif" text-anchor="middle" fill="#fff">R</text>`,
	"java":      `<path d="M2.8 7.2h8.4v3.4A3.6 3.6 0 0 1 7.6 14.2H6.4a3.6 3.6 0 0 1-3.6-3.6Z" fill="currentColor"/><path d="M11.2 8.2h.9a1.7 1.7 0 0 1 0 3.4h-1.3" fill="none" stroke="currentColor" stroke-width="1.3"/><path d="M5.4 5.6c-.9-1 .9-1.7 0-3M8.4 5.6c-.9-1 .9-1.7 0-3" fill="none" stroke="currentColor" stroke-width="1.2" stroke-linecap="round"/>`,
	"kotlin":    `<path d="M2 2h12L8 8l6 6H2Z" fill="currentColor"/><path d="M2 2h6L2 8Z" fill="#fff" fill-opacity=".3"/>`,
	"ruby":      `<path d="M4 2h8l3 4-7 8.5L1 6Z" fill="currentColor"/><path d="M1 6h14M5.4 6 8 14.5 10.6 6M4 2l1.4 4L8 2l2.6 4L12 2" fill="none" stroke="#fff" stroke-opacity=".5" stroke-width=".8"/>`,
	"react":     `<g fill="none" stroke="currentColor" stroke-width="1.1"><ellipse cx="8" cy="8" rx="7" ry="2.7"/><ellipse cx="8" cy="8" rx="7" ry="2.7" transform="rotate(60 8 8)"/><ellipse cx="8" cy="8" rx="7" ry="2.7" transform="rotate(120 8 8)"/></g><circle cx="8" cy="8" r="1.5" fill="currentColor"/>`,
	"vue":       `<path d="M.8 2.2h3.3L8 8.9l3.9-6.7h3.3L8 14.6Z" fill="#41b883"/><path d="M4.1 2.2h2.4L8 4.8l1.5-2.6h2.4L8 8.9Z" fill="#35495e"/>`,
	"drop":      `<path d="M8 1c3 4 5 6.6 5 9.1a5 5 0 0 1-10 0C3 7.6 5 5 8 1Z" fill="currentColor"/><path d="M6.4 8.4c-.9 1.5-.6 3.4.9 4.1" fill="none" stroke="#fff" stroke-opacity=".6" stroke-width="1" stroke-linecap="round"/>`,
	"lua":       `<circle cx="7.3" cy="8.7" r="6.3" fill="currentColor"/><circle cx="9.6" cy="6.4" r="1.8" fill="#fff"/><circle cx="13.9" cy="2.1" r="1.6" fill="currentColor"/>`,
	"julia":     `<circle cx="8" cy="4.3" r="3" fill="#389826"/><circle cx="4" cy="11.2" r="3" fill="#cb3c33"/><circle cx="12" cy="11.2" r="3" fill="#9558b2"/>`,
	"graphql":   `<path d="M8 1.6 13.5 4.8v6.4L8 14.4 2.5 11.2V4.8ZM8 1.6 2.9 10.9h10.2Z" fill="none" stroke="currentColor" stroke-width="1"/><g fill="currentColor"><circle cx="8" cy="1.6" r="1.3"/><circle cx="13.5" cy="4.8" r="1.3"/><circle cx="13.5" cy="11.2" r="1.3"/><circle cx="8" cy="14.4" r="1.3"/><circle cx="2.5" cy="11.2" r="1.3"/><circle cx="2.5" cy="4.8" r="1.3"/></g>`,
	"terraform": `<path d="M5.9.9l4 2.3v4.6l-4-2.3ZM10.4 3.5l4 2.3v4.6l-4-2.3ZM1.4.9 5.4 3.2v4.6L1.4 5.5ZM5.9 8.3l4 2.3v4.6l-4-2.3Z" fill="currentColor"/>`,
	"jupyter":   `<path d="M2.2 5.7C3.4 3.6 5.5 2.4 8 2.4s4.6 1.2 5.8 3.3C12.2 4.6 10.2 4 8 4s-4.2.6-5.8 1.7ZM2.2 10.3c1.2 2.1 3.3 3.3 5.8 3.3s4.6-1.2 5.8-3.3C12.2 11.4 10.2 12 8 12s-4.2-.6-5.8-1.7Z" fill="currentColor"/><circle cx="13.2" cy="2" r="1" fill="#8a8f98"/><circle cx="2.7" cy="13.6" r="1.2" fill="#8a8f98"/><circle cx="3" cy="2.6" r=".6" fill="#8a8f98"/>`,
	"diamond":   `<path d="M8 .8 12.6 8 8 10.7 3.4 8Z" fill="currentColor"/><path d="M8 11.8 12.6 9.1 8 15.2 3.4 9.1Z" fill="currentColor" fill-opacity=".7"/>`,
	"scala":     `<path d="M3.5 1.6c0 .8 9 1.3 9 2.3V6.5c0-1-9-1.5-9-2.3ZM3.5 6.3c0 .8 9 1.3 9 2.3v2.6c0-1-9-1.5-9-2.3ZM3.5 11c0 .8 9 1.3 9 2.3v1.4c0-1-9-1.5-9-2.3Z" fill="currentColor"/>`,
}

// shapes are backgrounds for a text label.
var shapes = map[string]string{
	"badge":   `<rect x="1" y="1" width="14" height="14" rx="3.2" fill="currentColor"/>`,
	"circle":  `<circle cx="8" cy="8" r="7" fill="currentColor"/>`,
	"hex":     `<path d="M8 .7l6.4 3.7v7.2L8 15.3l-6.4-3.7V4.4Z" fill="currentColor"/>`,
	"shield":  `<path d="M1.6 1h12.8l-1.2 12.3L8 15l-5.2-1.7Z" fill="currentColor"/><path d="M8 2.1v11.7l3.9-1.3 1-10.4Z" fill="#fff" fill-opacity=".16"/>`,
	"ellipse": `<ellipse cx="8" cy="8" rx="7.6" ry="5" fill="currentColor"/>`,
	"page":    `<path d="M4.25 1h5.2L13 4.55v8.7c0 .97-.78 1.75-1.75 1.75h-7C3.28 15 2.5 14.22 2.5 13.25V2.75C2.5 1.78 3.28 1 4.25 1Z" fill="currentColor"/><path d="M9.3 1.1v2.7c0 .5.4.9.9.9h2.7Z" fill="#fff" fill-opacity=".45"/>`,
	"corner":  `<rect x="1" y="1" width="14" height="14" rx="2" fill="currentColor"/>`,
}

// labelMarkup places the label inside its shape; long labels shrink.
func labelMarkup(k fileKind) string {
	fill := k.ink
	if fill == "" {
		fill = "#fff"
	}
	n := utf8.RuneCountInString(k.label)
	size := map[int]float64{1: 9, 2: 7.2, 3: 5.4, 4: 4.3}[min(n, 4)]
	cx, cy := 8.0, 8.0
	switch k.icon {
	case "page":
		cy, size = 10.6, min(size, 4.8)
	case "corner":
		cx, cy, size = 10.2, 11.2, 6.4
	case "shield":
		cy = 7.4
	}
	fit := ""
	if n >= 3 {
		width := 11.6
		if k.icon == "page" {
			width = 8.2
		}
		fit = fmt.Sprintf(` textLength="%.1f" lengthAdjust="spacingAndGlyphs"`, width)
	}
	return fmt.Sprintf(`<text x="%.1f" y="%.2f" font-size="%.1f" font-weight="700" font-family="Onest,Arial,sans-serif" text-anchor="middle"%s fill="%s">%s</text>`,
		cx, cy+size*0.36, size, fit, fill, template.HTMLEscapeString(k.label))
}

var byExt = map[string]fileKind{
	// systems and backend languages
	"go":     lbl("badge", "#00add8", "GO"),
	"rs":     sym("rust", "#ce6a2b"),
	"py":     sym("python", ""),
	"pyi":    sym("python", ""),
	"pyw":    sym("python", ""),
	"java":   sym("java", "#e76f00"),
	"kt":     sym("kotlin", "#a97bff"),
	"kts":    sym("kotlin", "#7f52ff"),
	"scala":  sym("scala", "#dc322f"),
	"sc":     sym("scala", "#dc322f"),
	"groovy": lbl("circle", "#4298b8", "G"),
	"gradle": sym("cube", "#3d7a8c"),
	"c":      lbl("hex", "#5c6bc0", "C"),
	"h":      lbl("hex", "#a074c4", "H"),
	"cc":     lbl("hex", "#f34b7d", "C++"),
	"cpp":    lbl("hex", "#f34b7d", "C++"),
	"cxx":    lbl("hex", "#f34b7d", "C++"),
	"hpp":    lbl("hex", "#c2185b", "H++"),
	"cs":     lbl("hex", "#9b4f96", "C#"),
	"fs":     lbl("hex", "#378bba", "F#"),
	"vb":     lbl("badge", "#945db7", "VB"),
	"m":      lbl("badge", "#438eff", "OC"),
	"swift":  lbl("badge", "#f05138", "S"),
	"dart":   lbl("circle", "#0175c2", "D"),
	"rb":     sym("ruby", "#cc342d"),
	"erb":    sym("ruby", "#a8211b"),
	"php":    lbl("ellipse", "#777bb4", "php"),
	"pl":     lbl("badge", "#0298c3", "PL"),
	"pm":     lbl("badge", "#0298c3", "PM"),
	"lua":    sym("lua", "#4b4bd6"),
	"ex":     sym("drop", "#8e5fa8"),
	"exs":    sym("drop", "#a472bd"),
	"erl":    lbl("badge", "#b83998", "E"),
	"hs":     lbl("badge", "#7b67b5", "λ"),
	"clj":    lbl("circle", "#5881d8", "λ"),
	"cljs":   lbl("circle", "#63b132", "λ"),
	"ml":     lbl("badge", "#ec6813", "ML"),
	"zig":    ink("badge", "#f7a41d", "Z", "#1b1b1b"),
	"nim":    ink("badge", "#ffe953", "N", "#1b1b1b"),
	"cr":     lbl("badge", "#2e2e2e", "CR"),
	"v":      lbl("badge", "#5d87bf", "V"),
	"r":      lbl("circle", "#276dc3", "R"),
	"jl":     sym("julia", ""),
	"sol":    sym("diamond", "#6f6f9f"),
	"asm":    lbl("badge", "#6e4c13", "ASM"),
	"s":      lbl("badge", "#6e4c13", "ASM"),
	"wasm":   lbl("badge", "#654ff0", "WA"),
	"wat":    lbl("badge", "#654ff0", "WA"),
	// web
	"js":      ink("corner", "#f0db4f", "JS", "#1b1b1b"),
	"mjs":     ink("corner", "#f0db4f", "JS", "#1b1b1b"),
	"cjs":     ink("corner", "#f0db4f", "JS", "#1b1b1b"),
	"ts":      lbl("corner", "#3178c6", "TS"),
	"mts":     lbl("corner", "#3178c6", "TS"),
	"cts":     lbl("corner", "#3178c6", "TS"),
	"jsx":     sym("react", "#149eca"),
	"tsx":     sym("react", "#3178c6"),
	"vue":     sym("vue", ""),
	"svelte":  lbl("badge", "#ff3e00", "S"),
	"astro":   lbl("badge", "#bc52ee", "A"),
	"html":    lbl("shield", "#e34c26", "5"),
	"htm":     lbl("shield", "#e34c26", "5"),
	"css":     lbl("shield", "#1572b6", "3"),
	"scss":    lbl("circle", "#c6538c", "S"),
	"sass":    lbl("circle", "#c6538c", "S"),
	"less":    lbl("circle", "#2f5b9a", "L"),
	"styl":    lbl("circle", "#3eaf7c", "S"),
	"graphql": sym("graphql", "#e10098"),
	"gql":     sym("graphql", "#e10098"),
	"wgsl":    lbl("badge", "#005a9c", "GL"),
	"glsl":    lbl("badge", "#5586a4", "GL"),
	"tmpl":    lbl("badge", "#00add8", "{{"),
	"gotmpl":  lbl("badge", "#00add8", "{{"),
	"hbs":     lbl("badge", "#f0772b", "{{"),
	"j2":      lbl("badge", "#b41717", "{{"),
	"jinja":   lbl("badge", "#b41717", "{{"),
	// infrastructure
	"tf":     sym("terraform", "#7b42bc"),
	"tfvars": sym("terraform", "#5c4ee5"),
	"hcl":    sym("terraform", "#7b42bc"),
	"nix":    lbl("badge", "#5277c3", "NX"),
	"proto":  lbl("badge", "#4a8f5c", "PB"),
	"ipynb":  sym("jupyter", "#f37626"),
	"rego":   lbl("badge", "#7d9199", "OPA"),
	// shells and scripts
	"sh":   sym("terminal", "#4eaa25"),
	"bash": sym("terminal", "#4eaa25"),
	"zsh":  sym("terminal", "#3b8a1e"),
	"fish": sym("terminal", "#4aae47"),
	"ps1":  sym("terminal", "#2c7ad6"),
	"psm1": sym("terminal", "#2c7ad6"),
	"bat":  sym("terminal", "#5b6270"),
	"cmd":  sym("terminal", "#5b6270"),
	"awk":  sym("terminal", "#c28c00"),
	// data and config
	"json":       sym("braces", "#d4a20a"),
	"jsonc":      sym("braces", "#d4a20a"),
	"json5":      sym("braces", "#c78c06"),
	"jsonl":      sym("braces", "#b57d00"),
	"ndjson":     sym("braces", "#b57d00"),
	"yml":        sym("bars", "#cb171e"),
	"yaml":       sym("bars", "#cb171e"),
	"toml":       sym("brackets", "#9c4221"),
	"ini":        sym("sliders", "#7a869a"),
	"cfg":        sym("sliders", "#7a869a"),
	"conf":       sym("sliders", "#6d7a8f"),
	"properties": sym("sliders", "#7a869a"),
	"env":        sym("sliders", "#c9a400"),
	"xml":        sym("angle", "#0060ac"),
	"xsd":        sym("angle", "#0060ac"),
	"xsl":        sym("angle", "#33a9dc"),
	"plist":      sym("angle", "#8a8f98"),
	"sql":        sym("cylinder", "#e38c00"),
	"db":         sym("cylinder", "#3b8bd0"),
	"sqlite":     sym("cylinder", "#0f80cc"),
	"sqlite3":    sym("cylinder", "#0f80cc"),
	"prisma":     sym("cylinder", "#2d3748"),
	"csv":        sym("sheet", "#237346"),
	"tsv":        sym("sheet", "#2e8b57"),
	"parquet":    sym("sheet", "#50abf1"),
	"avro":       sym("sheet", "#c22d40"),
	// documents
	"md":       sym("md", "#3d8fd1"),
	"markdown": sym("md", "#3d8fd1"),
	"mdx":      sym("md", "#f9ac00"),
	"rst":      sym("lines", "#3d8fd1"),
	"adoc":     sym("lines", "#e40046"),
	"txt":      sym("lines", "#7a869a"),
	"text":     sym("lines", "#7a869a"),
	"log":      sym("lines", "#a37b45"),
	"out":      sym("lines", "#a37b45"),
	"diff":     sym("lines", "#41b883"),
	"patch":    sym("lines", "#41b883"),
	"pdf":      lbl("page", "#d0342c", "PDF"),
	"doc":      lbl("page", "#2b579a", "DOC"),
	"docx":     lbl("page", "#2b579a", "DOC"),
	"odt":      lbl("page", "#1e88c9", "ODT"),
	"rtf":      lbl("page", "#6d7a8f", "RTF"),
	"xls":      lbl("page", "#217346", "XLS"),
	"xlsx":     lbl("page", "#217346", "XLS"),
	"ods":      lbl("page", "#10a37f", "ODS"),
	"ppt":      lbl("page", "#d24726", "PPT"),
	"pptx":     lbl("page", "#d24726", "PPT"),
	"tex":      lbl("page", "#3d6117", "TEX"),
	"epub":     sym("book", "#86b918"),
	// images and design
	"png":    sym("picture", "#a061c9"),
	"jpg":    sym("picture", "#d6723a"),
	"jpeg":   sym("picture", "#d6723a"),
	"gif":    sym("picture", "#e05297"),
	"webp":   sym("picture", "#4285f4"),
	"avif":   sym("picture", "#2d9d78"),
	"bmp":    sym("picture", "#6d7a8f"),
	"tif":    sym("picture", "#7b6fb0"),
	"tiff":   sym("picture", "#7b6fb0"),
	"heic":   sym("picture", "#607d8b"),
	"ico":    sym("picture", "#26a69a"),
	"svg":    sym("vector", "#e89b10"),
	"psd":    ink("badge", "#001e36", "Ps", "#31a8ff"),
	"ai":     ink("badge", "#330000", "Ai", "#ff9a00"),
	"fig":    lbl("badge", "#a259ff", "F"),
	"sketch": sym("diamond", "#fdb300"),
	"xcf":    lbl("badge", "#5c5543", "G"),
	// audio
	"mp3":  sym("note", "#e91e63"),
	"wav":  sym("note", "#1e88e5"),
	"flac": sym("note", "#ff7043"),
	"ogg":  sym("note", "#26a69a"),
	"opus": sym("note", "#00897b"),
	"m4a":  sym("note", "#8e24aa"),
	"aac":  sym("note", "#8e24aa"),
	"aiff": sym("note", "#5c6bc0"),
	"mid":  sym("note", "#6d7a8f"),
	"midi": sym("note", "#6d7a8f"),
	// video
	"mp4":  sym("film", "#e53935"),
	"m4v":  sym("film", "#e53935"),
	"mov":  sym("film", "#1e88e5"),
	"webm": sym("film", "#43a047"),
	"mkv":  sym("film", "#7e57c2"),
	"avi":  sym("film", "#fb8c00"),
	"wmv":  sym("film", "#00897b"),
	"flv":  sym("film", "#d81b60"),
	// fonts
	"woff":  sym("font", "#d45d79"),
	"woff2": sym("font", "#d45d79"),
	"ttf":   sym("font", "#8e5fa8"),
	"otf":   sym("font", "#3d8fd1"),
	"eot":   sym("font", "#6d7a8f"),
	// archives and packages
	"zip":   sym("box", "#b8862b"),
	"tar":   sym("box", "#8d6e63"),
	"gz":    sym("box", "#9c7a3c"),
	"tgz":   sym("box", "#9c7a3c"),
	"bz2":   sym("box", "#a1887f"),
	"xz":    sym("box", "#795548"),
	"zst":   sym("box", "#6d4c41"),
	"7z":    sym("box", "#5e7d3a"),
	"rar":   sym("box", "#7b1fa2"),
	"jar":   sym("box", "#b07219"),
	"war":   sym("box", "#b07219"),
	"deb":   sym("box", "#a80030"),
	"rpm":   sym("box", "#ee0000"),
	"apk":   sym("box", "#3ddc84"),
	"whl":   sym("box", "#3776ab"),
	"gem":   sym("box", "#cc342d"),
	"nupkg": sym("box", "#004880"),
	// binaries and disk images
	"exe":   sym("chip", "#5b6270"),
	"dll":   sym("chip", "#5b6270"),
	"so":    sym("chip", "#607d8b"),
	"dylib": sym("chip", "#607d8b"),
	"o":     sym("chip", "#78909c"),
	"a":     sym("chip", "#78909c"),
	"bin":   sym("chip", "#546e7a"),
	"class": sym("chip", "#b07219"),
	"pyc":   sym("chip", "#3776ab"),
	"iso":   sym("disc", "#607d8b"),
	"dmg":   sym("disc", "#8a8f98"),
	"img":   sym("disc", "#78909c"),
	// keys, certificates, locks
	"key":  sym("key", "#8a8f98"),
	"pem":  sym("key", "#c9a227"),
	"crt":  sym("key", "#2e7d32"),
	"cer":  sym("key", "#2e7d32"),
	"csr":  sym("key", "#1565c0"),
	"p12":  sym("key", "#6a1b9a"),
	"pub":  sym("key", "#00897b"),
	"gpg":  sym("key", "#0093dd"),
	"asc":  sym("key", "#0093dd"),
	"lock": sym("padlock", "#8a8f98"),
	"sum":  sym("padlock", "#8a8f98"),
}

var byName = map[string]fileKind{
	"dockerfile":          sym("whale", "#2496ed"),
	"containerfile":       sym("whale", "#892ca0"),
	".dockerignore":       sym("whale", "#8a8f98"),
	"docker-compose.yml":  sym("whale", "#1d63ed"),
	"docker-compose.yaml": sym("whale", "#1d63ed"),
	"compose.yml":         sym("whale", "#1d63ed"),
	"compose.yaml":        sym("whale", "#1d63ed"),
	"makefile":            sym("gear", "#6d8086"),
	"gnumakefile":         sym("gear", "#6d8086"),
	"justfile":            sym("gear", "#c78c06"),
	"cmakelists.txt":      sym("gear", "#064f8c"),
	"build":               sym("gear", "#43a047"),
	"build.bazel":         sym("gear", "#43a047"),
	"workspace":           sym("gear", "#43a047"),
	"jenkinsfile":         sym("gear", "#d24939"),
	"procfile":            sym("gear", "#7a5fb3"),
	"taskfile.yml":        sym("gear", "#29beb0"),
	"vagrantfile":         lbl("badge", "#1563ff", "V"),
	"go.mod":              sym("cube", "#00add8"),
	"go.work":             sym("cube", "#00add8"),
	"go.sum":              sym("padlock", "#00add8"),
	"package.json":        sym("cube", "#cb3837"),
	"package-lock.json":   sym("padlock", "#cb3837"),
	"yarn.lock":           sym("padlock", "#2c8ebb"),
	"pnpm-lock.yaml":      sym("padlock", "#f69220"),
	"bun.lockb":           sym("padlock", "#f472b6"),
	"cargo.toml":          sym("cube", "#ce6a2b"),
	"cargo.lock":          sym("padlock", "#ce6a2b"),
	"pyproject.toml":      sym("cube", "#3776ab"),
	"setup.py":            sym("cube", "#3776ab"),
	"requirements.txt":    sym("cube", "#3776ab"),
	"pipfile":             sym("cube", "#3776ab"),
	"poetry.lock":         sym("padlock", "#3776ab"),
	"uv.lock":             sym("padlock", "#de5fe9"),
	"gemfile":             sym("cube", "#cc342d"),
	"gemfile.lock":        sym("padlock", "#cc342d"),
	"composer.json":       sym("cube", "#885630"),
	"composer.lock":       sym("padlock", "#885630"),
	"pom.xml":             sym("cube", "#c71a36"),
	"build.gradle":        sym("cube", "#3d7a8c"),
	"build.gradle.kts":    sym("cube", "#7f52ff"),
	"tsconfig.json":       sym("sliders", "#3178c6"),
	"jsconfig.json":       sym("sliders", "#c9a400"),
	".eslintrc":           sym("sliders", "#4b32c3"),
	".eslintrc.json":      sym("sliders", "#4b32c3"),
	"eslint.config.js":    sym("sliders", "#4b32c3"),
	".prettierrc":         sym("sliders", "#c596c7"),
	".editorconfig":       sym("sliders", "#7a869a"),
	".npmrc":              sym("sliders", "#cb3837"),
	".nvmrc":              sym("sliders", "#43a047"),
	".env":                sym("sliders", "#c9a400"),
	".gitignore":          sym("git", "#f05032"),
	".gitattributes":      sym("git", "#f05032"),
	".gitmodules":         sym("git", "#f05032"),
	".gitkeep":            sym("git", "#f05032"),
	".mailmap":            sym("git", "#f05032"),
	"codeowners":          sym("people", "#3b5bdb"),
	"owners":              sym("people", "#3b5bdb"),
	"authors":             sym("people", "#7a5fb3"),
	"contributors":        sym("people", "#7a5fb3"),
	"changelog.md":        sym("book", "#e65100"),
	"changes.md":          sym("book", "#e65100"),
	"contributing.md":     sym("book", "#43a047"),
	"security.md":         sym("padlock", "#d0342c"),
	"robots.txt":          sym("lines", "#43a047"),
}

var (
	fileDefault = sym("plain", "")
	licenseKind = sym("ribbon", "#c9a227")
	readmeKind  = sym("book", "#3d8fd1")
)

// isTest recognises test files by the usual naming conventions.
func isTest(lower, ext string) bool {
	stem := strings.TrimSuffix(lower, "."+ext)
	switch ext {
	case "go", "py", "rs", "rb", "ex", "exs":
		return strings.HasSuffix(stem, "_test") || strings.HasPrefix(stem, "test_") || strings.HasSuffix(stem, "_spec")
	case "js", "ts", "jsx", "tsx", "mjs", "cjs", "mts":
		return strings.HasSuffix(stem, ".test") || strings.HasSuffix(stem, ".spec")
	case "java", "kt", "cs", "scala", "swift", "php":
		return strings.HasSuffix(stem, "test") || strings.HasSuffix(stem, "tests") || strings.HasSuffix(stem, "spec")
	}
	return false
}

func kindOf(name string) fileKind {
	lower := strings.ToLower(path.Base(name))
	if k, ok := byName[lower]; ok {
		return k
	}
	ext := strings.TrimPrefix(path.Ext(lower), ".")
	stem := strings.TrimSuffix(lower, path.Ext(lower))
	switch {
	case stem == "readme":
		return readmeKind
	case stem == "license" || stem == "licence" || stem == "copying" || stem == "notice" || strings.HasPrefix(lower, "license-"):
		return licenseKind
	case strings.HasPrefix(lower, "dockerfile.") || strings.HasSuffix(lower, ".dockerfile"):
		return byName["dockerfile"]
	case strings.HasPrefix(lower, ".env."):
		return byName[".env"]
	case strings.HasPrefix(lower, "docker-compose.") && (ext == "yml" || ext == "yaml"):
		return byName["docker-compose.yml"]
	case strings.HasPrefix(lower, "makefile."):
		return byName["makefile"]
	}
	if k, ok := byExt[ext]; ok {
		if isTest(lower, ext) {
			c := k.color
			if c == "" { // two-colour marks (python, vue, julia) have no single colour
				c = "#3776ab"
			}
			return sym("flask", c)
		}
		return k
	}
	return fileDefault
}

// renderKind returns the inner SVG markup for a kind.
func renderKind(k fileKind) string {
	if s, ok := shapes[k.icon]; ok {
		return s + labelMarkup(k)
	}
	return symbols[k.icon]
}

// fileIcon renders the icon for a file name (a path works too).
func fileIcon(name string) template.HTML {
	k := kindOf(name)
	style := ""
	if k.color != "" {
		style = fmt.Sprintf(` style="--fc:%s"`, k.color)
	}
	return template.HTML(fmt.Sprintf(`<svg class="icon ficon ficon-%s"%s viewBox="0 0 16 16" width="16" height="16" aria-hidden="true">%s</svg>`,
		k.icon, style, renderKind(k)))
}

// dirIcon renders a folder icon; dot-directories (.github, .onegit) are muted.
func dirIcon(name string) template.HTML {
	cls := "icon icon-folder"
	if strings.HasPrefix(path.Base(name), ".") {
		cls += " icon-folder-dot"
	}
	return template.HTML(`<svg class="` + cls + `" viewBox="0 0 16 16" width="16" height="16" aria-hidden="true"><path fill="currentColor" d="M1.75 1A1.75 1.75 0 0 0 0 2.75v10.5C0 14.216.784 15 1.75 15h12.5A1.75 1.75 0 0 0 16 13.25v-8.5A1.75 1.75 0 0 0 14.25 3H7.5a.25.25 0 0 1-.2-.1l-.9-1.2C6.07 1.26 5.55 1 5 1H1.75Z"/></svg>`)
}
