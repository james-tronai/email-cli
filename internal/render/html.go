package render

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// HTMLToText converts an HTML body to plain text, dropping content a human
// reader would not see (display:none, visibility:hidden, zero/tiny font size,
// zero opacity, text coloured like its background, off-screen indents, hidden
// attribute, mso-hide). This is the defence against "white text" prompt
// injection: the model should read what the human reads.
//
// removed reports whether any non-whitespace hidden text was dropped.
func HTMLToText(src string) (text string, removed bool) {
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		return "", false
	}
	w := &textWriter{}
	walk(doc, w, "", &removed)
	return w.String(), removed
}

var skipElements = map[atom.Atom]bool{
	atom.Head: true, atom.Script: true, atom.Style: true, atom.Template: true,
	atom.Noscript: true, atom.Title: true, atom.Meta: true, atom.Link: true,
	atom.Object: true, atom.Iframe: true, atom.Svg: true, atom.Math: true,
}

var blockElements = map[atom.Atom]bool{
	atom.P: true, atom.Div: true, atom.Section: true, atom.Article: true,
	atom.Header: true, atom.Footer: true, atom.Blockquote: true, atom.Pre: true,
	atom.Table: true, atom.Tr: true, atom.Ul: true, atom.Ol: true,
	atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true, atom.H5: true, atom.H6: true,
	atom.Hr: true, atom.Center: true, atom.Address: true, atom.Dl: true, atom.Dt: true, atom.Dd: true,
	atom.Form: true, atom.Fieldset: true, atom.Figure: true, atom.Main: true, atom.Nav: true, atom.Aside: true,
}

// bg is the effective background colour inherited from ancestors ("" = unknown,
// treated as white for the white-on-white check).
func walk(n *html.Node, w *textWriter, bg string, removed *bool) {
	switch n.Type {
	case html.TextNode:
		w.text(n.Data)
		return
	case html.CommentNode, html.DoctypeNode:
		return
	case html.ElementNode:
		if skipElements[n.DataAtom] {
			return
		}
		st := parseStyle(attr(n, "style"))
		if b := firstNonEmpty(st["background-color"], colorFromBackground(st["background"]), attr(n, "bgcolor")); b != "" {
			bg = normColor(b)
		}
		if isHidden(n, st, bg) {
			if strings.TrimSpace(textContent(n)) != "" {
				*removed = true
			}
			return
		}
		switch n.DataAtom {
		case atom.Br:
			w.newline()
			return
		case atom.Img:
			if alt := strings.TrimSpace(attr(n, "alt")); alt != "" {
				w.text("[image: " + alt + "]")
			}
			return
		case atom.Li:
			w.block()
			w.text("- ")
		case atom.Td, atom.Th:
			w.text(" ")
		}
		if blockElements[n.DataAtom] {
			w.block()
		}
		if n.DataAtom == atom.Pre {
			w.pre++
		}
	}

	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walk(c, w, bg, removed)
	}

	if n.Type == html.ElementNode {
		if n.DataAtom == atom.Pre {
			w.pre--
		}
		if n.DataAtom == atom.A {
			href := strings.TrimSpace(attr(n, "href"))
			if href != "" && !strings.HasPrefix(href, "#") && !strings.Contains(textContent(n), href) {
				w.text(" <" + href + ">")
			}
		}
		if blockElements[n.DataAtom] || n.DataAtom == atom.Li {
			w.block()
		}
	}
}

func isHidden(n *html.Node, st map[string]string, bg string) bool {
	if _, ok := attrOK(n, "hidden"); ok {
		return true
	}
	if strings.EqualFold(attr(n, "type"), "hidden") {
		return true
	}
	if v := st["display"]; v == "none" {
		return true
	}
	if v := st["visibility"]; v == "hidden" || v == "collapse" {
		return true
	}
	if st["mso-hide"] == "all" {
		return true
	}
	if v, ok := st["opacity"]; ok {
		if f, err := strconv.ParseFloat(strings.TrimSuffix(v, "%"), 64); err == nil && f <= 0.05 {
			return true
		}
	}
	if v, ok := st["font-size"]; ok && tinyLength(v) {
		return true
	}
	if n.DataAtom == atom.Font {
		if sz := attr(n, "size"); sz == "0" {
			return true
		}
	}
	for _, k := range []string{"max-height", "height", "max-width", "width"} {
		if v, ok := st[k]; ok && zeroLength(v) && (st["overflow"] == "hidden" || k == "max-height" || k == "max-width") {
			return true
		}
	}
	if v, ok := st["text-indent"]; ok && farOffscreen(v) {
		return true
	}
	for _, k := range []string{"left", "top"} {
		if v, ok := st[k]; ok && farOffscreen(v) && (st["position"] == "absolute" || st["position"] == "fixed") {
			return true
		}
	}
	if st["clip"] == "rect(0,0,0,0)" || st["clip"] == "rect(0px,0px,0px,0px)" || st["clip-path"] == "inset(50%)" {
		return true
	}
	fg := firstNonEmpty(st["color"], attr(n, "color"))
	if fg != "" {
		effBg := bg
		if effBg == "" {
			effBg = "#ffffff"
		}
		if sameColor(normColor(fg), effBg) {
			return true
		}
	}
	return false
}

var lengthRE = regexp.MustCompile(`^(-?[0-9.]+)\s*(px|pt|em|rem|%|)$`)

func parseLength(v string) (float64, string, bool) {
	m := lengthRE.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return 0, "", false
	}
	f, err := strconv.ParseFloat(m[1], 64)
	return f, m[2], err == nil
}

func zeroLength(v string) bool {
	f, _, ok := parseLength(v)
	return ok && f == 0
}

// tinyLength: font sizes nobody can read (<= 2px / 1.5pt / 0.15em).
func tinyLength(v string) bool {
	f, unit, ok := parseLength(v)
	if !ok {
		return false
	}
	switch unit {
	case "px", "":
		return f <= 2
	case "pt":
		return f <= 1.5
	case "em", "rem":
		return f <= 0.15
	case "%":
		return f <= 15
	}
	return false
}

func farOffscreen(v string) bool {
	f, _, ok := parseLength(v)
	return ok && f <= -500
}

func parseStyle(s string) map[string]string {
	out := map[string]string{}
	for _, decl := range strings.Split(s, ";") {
		k, v, ok := strings.Cut(decl, ":")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "!important")))
		v = strings.TrimSpace(v)
		v = strings.Join(strings.Fields(v), "")
		if k != "" {
			out[k] = v
		}
	}
	return out
}

var namedColors = map[string]string{
	"white": "#ffffff", "black": "#000000", "red": "#ff0000", "green": "#008000",
	"blue": "#0000ff", "yellow": "#ffff00", "gray": "#808080", "grey": "#808080",
	"silver": "#c0c0c0", "whitesmoke": "#f5f5f5", "snow": "#fffafa", "ivory": "#fffff0",
}

var rgbRE = regexp.MustCompile(`^rgba?\((\d+),(\d+),(\d+)(?:,([0-9.]+))?\)$`)

// normColor returns "#rrggbb", "transparent", or the input lower-cased.
func normColor(c string) string {
	c = strings.ToLower(strings.Join(strings.Fields(c), ""))
	if v, ok := namedColors[c]; ok {
		return v
	}
	if c == "transparent" {
		return c
	}
	if strings.HasPrefix(c, "#") {
		h := c[1:]
		if len(h) == 3 || len(h) == 4 {
			h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
		}
		if len(h) >= 6 {
			return "#" + h[:6]
		}
		return c
	}
	if m := rgbRE.FindStringSubmatch(c); m != nil {
		if m[4] != "" {
			if a, err := strconv.ParseFloat(m[4], 64); err == nil && a <= 0.05 {
				return "transparent"
			}
		}
		var b strings.Builder
		b.WriteByte('#')
		for _, p := range m[1:4] {
			v, _ := strconv.Atoi(p)
			b.WriteString(strconv.FormatInt(int64(min(v, 255)|0x100), 16)[1:])
		}
		return b.String()
	}
	return c
}

// sameColor is true when fg is transparent or within a small distance of bg.
func sameColor(fg, bg string) bool {
	if fg == "transparent" {
		return true
	}
	if fg == bg {
		return true
	}
	r1, g1, b1, ok1 := hexRGB(fg)
	r2, g2, b2, ok2 := hexRGB(bg)
	if !ok1 || !ok2 {
		return false
	}
	d := abs(r1-r2) + abs(g1-g2) + abs(b1-b2)
	return d <= 24
}

func hexRGB(c string) (int, int, int, bool) {
	if len(c) != 7 || c[0] != '#' {
		return 0, 0, 0, false
	}
	v, err := strconv.ParseUint(c[1:], 16, 32)
	if err != nil {
		return 0, 0, 0, false
	}
	return int(v >> 16 & 0xff), int(v >> 8 & 0xff), int(v & 0xff), true
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func colorFromBackground(v string) string {
	for _, f := range strings.Fields(strings.ReplaceAll(v, ",", ", ")) {
		if c := normColor(f); strings.HasPrefix(c, "#") || c == "transparent" {
			return c
		}
	}
	if m := regexp.MustCompile(`rgba?\([^)]*\)`).FindString(v); m != "" {
		return m
	}
	return ""
}

func attr(n *html.Node, key string) string {
	v, _ := attrOK(n, key)
	return v
}

func attrOK(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val, true
		}
	}
	return "", false
}

func textContent(n *html.Node) string {
	var b strings.Builder
	var f func(*html.Node)
	f = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		if n.Type == html.ElementNode && skipElements[n.DataAtom] {
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			f(c)
		}
	}
	f(n)
	return b.String()
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// textWriter collapses whitespace like a browser, with block breaks.
type textWriter struct {
	b       strings.Builder
	pre     int
	pending int  // newlines owed before next text (max 2)
	space   bool // a space is owed before next text
}

func (w *textWriter) text(s string) {
	if s == "" {
		return
	}
	if w.pre > 0 {
		w.flush()
		w.b.WriteString(s)
		return
	}
	first, _ := utf8.DecodeRuneInString(s)
	last, _ := utf8.DecodeLastRuneInString(s)
	words := strings.FieldsFunc(s, isSpace)
	if isSpace(first) {
		w.space = true
	}
	for i, word := range words {
		if (i > 0 || w.space) && w.pending == 0 && w.b.Len() > 0 {
			w.b.WriteByte(' ')
		}
		w.flush()
		w.b.WriteString(word)
	}
	w.space = len(words) == 0 || isSpace(last)
}

func (w *textWriter) flush() {
	if w.b.Len() > 0 {
		for range w.pending {
			w.b.WriteByte('\n')
		}
	}
	w.pending = 0
}

func (w *textWriter) newline() {
	w.pending = min(w.pending+1, 2)
	w.space = false
}

func (w *textWriter) block() {
	if w.pending < 1 {
		w.pending = 1
	}
	w.space = false
}

func (w *textWriter) String() string {
	lines := strings.Split(w.b.String(), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func isSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' || r == ' '
}
