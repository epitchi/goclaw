package tools

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// extractJSON pretty-prints JSON content.
func extractJSON(body []byte) (string, string) {
	var data interface{}
	if err := json.Unmarshal(body, &data); err == nil {
		formatted, _ := json.MarshalIndent(data, "", "  ")
		return string(formatted), "json"
	}
	return string(body), "raw"
}

// --- DOM-based HTML extraction ---

type convertMode int

const (
	modeMarkdown convertMode = iota
	modeText
)

// converter walks a parsed HTML DOM tree and emits markdown or plain text.
type converter struct {
	buf       strings.Builder
	mode      convertMode
	inPre     bool
	listDepth int
	listType  []atom.Atom // stack: atom.Ul / atom.Ol
	listIndex []int       // ordered list counters
	inLink    bool
}

// Elements to skip entirely (element + all descendants).
var skipElements = map[atom.Atom]bool{
	atom.Head:     true,
	atom.Script:   true,
	atom.Style:    true,
	atom.Noscript: true,
	atom.Svg:      true,
	atom.Template: true,
	atom.Iframe:   true,
	atom.Select:   true,
	atom.Option:   true,
	atom.Button:   true,
	atom.Input:    true,
	atom.Form:     true,
	atom.Nav:      true,
	atom.Footer:   true,
	atom.Picture:  true,
	atom.Source:   true,
}

// Additional elements to skip in text mode only.
var skipInTextMode = map[atom.Atom]bool{
	atom.Header: true,
	atom.Aside:  true,
}

// Block elements that need surrounding newlines.
var blockElements = map[atom.Atom]bool{
	atom.P: true, atom.Div: true, atom.Section: true, atom.Article: true,
	atom.Main: true, atom.H1: true, atom.H2: true, atom.H3: true,
	atom.H4: true, atom.H5: true, atom.H6: true, atom.Blockquote: true,
	atom.Pre: true, atom.Ul: true, atom.Ol: true, atom.Li: true,
	atom.Table: true, atom.Tr: true, atom.Hr: true, atom.Dl: true,
	atom.Dt: true, atom.Dd: true, atom.Figure: true, atom.Figcaption: true,
	atom.Details: true, atom.Summary: true, atom.Address: true,
}

// htmlToMarkdown converts HTML to a markdown-like format using DOM parsing.
func htmlToMarkdown(rawHTML string) string {
	doc, err := html.Parse(strings.NewReader(rawHTML))
	if err != nil {
		return stripTagsFallback(rawHTML)
	}
	body := findBody(doc)
	c := &converter{mode: modeMarkdown}
	c.walkChildren(body)
	return cleanOutput(c.buf.String())
}

// htmlToText extracts plain text from HTML content using DOM parsing.
func htmlToText(rawHTML string) string {
	doc, err := html.Parse(strings.NewReader(rawHTML))
	if err != nil {
		return stripTagsFallback(rawHTML)
	}
	body := findBody(doc)
	c := &converter{mode: modeText}
	c.walkChildren(body)
	return cleanTextOutput(c.buf.String())
}

func (c *converter) walk(n *html.Node) {
	switch n.Type {
	case html.TextNode:
		c.handleText(n)
		return
	case html.ElementNode:
		// handled below
	case html.DocumentNode:
		c.walkChildren(n)
		return
	default:
		return
	}

	tag := n.DataAtom

	if skipElements[tag] {
		return
	}
	if c.mode == modeText && skipInTextMode[tag] {
		return
	}

	switch tag {
	case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
		c.handleHeading(n)
	case atom.P:
		c.handleParagraph(n)
	case atom.A:
		c.handleLink(n)
	case atom.Img:
		c.handleImage(n)
	case atom.Pre:
		c.handlePre(n)
	case atom.Code:
		c.handleCode(n)
	case atom.Blockquote:
		c.handleBlockquote(n)
	case atom.Strong, atom.B:
		c.handleStrong(n)
	case atom.Em, atom.I:
		c.handleEmphasis(n)
	case atom.Br:
		c.buf.WriteByte('\n')
	case atom.Hr:
		c.ensureNewline()
		if c.mode == modeMarkdown {
			c.buf.WriteString("---\n")
		}
	case atom.Ul, atom.Ol:
		c.handleList(n)
	case atom.Li:
		c.handleListItem(n)
	case atom.Table:
		c.handleTable(n)
	case atom.Dt:
		c.handleDefinitionTerm(n)
	case atom.Dd:
		c.handleDefinitionDesc(n)
	default:
		if blockElements[tag] {
			c.ensureNewline()
			c.walkChildren(n)
			c.ensureNewline()
		} else {
			c.walkChildren(n)
		}
	}
}

func (c *converter) walkChildren(n *html.Node) {
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		c.walk(child)
	}
}

func (c *converter) handleText(n *html.Node) {
	text := n.Data
	if c.inPre {
		c.buf.WriteString(text)
		return
	}
	text = collapseWhitespace(text)
	if text == "" {
		return
	}
	if text == " " && c.buf.Len() > 0 {
		c.buf.WriteByte(' ')
		return
	}
	c.buf.WriteString(text)
}

func (c *converter) handleHeading(n *html.Node) {
	c.ensureDoubleNewline()
	if c.mode == modeMarkdown && len(n.Data) == 2 && n.Data[0] == 'h' {
		level := int(n.Data[1] - '0')
		for i := 0; i < level; i++ {
			c.buf.WriteByte('#')
		}
		c.buf.WriteByte(' ')
	}
	c.walkChildren(n)
	c.buf.WriteByte('\n')
}

func (c *converter) handleParagraph(n *html.Node) {
	c.ensureDoubleNewline()
	c.walkChildren(n)
	c.buf.WriteByte('\n')
}

func (c *converter) handleLink(n *html.Node) {
	href := getAttr(n, "href")
	if c.mode == modeText || c.inLink || href == "" || strings.HasPrefix(href, "javascript:") {
		c.walkChildren(n)
		return
	}
	c.inLink = true
	c.buf.WriteByte('[')
	c.walkChildren(n)
	c.buf.WriteString("](")
	c.buf.WriteString(href)
	c.buf.WriteByte(')')
	c.inLink = false
}

func (c *converter) handleImage(n *html.Node) {
	alt := getAttr(n, "alt")
	src := getAttr(n, "src")
	if c.mode == modeMarkdown {
		c.buf.WriteString("![")
		c.buf.WriteString(alt)
		c.buf.WriteByte(']')
		if src != "" {
			c.buf.WriteByte('(')
			c.buf.WriteString(src)
			c.buf.WriteByte(')')
		}
	} else if alt != "" {
		c.buf.WriteString(alt)
	}
}

func (c *converter) handlePre(n *html.Node) {
	c.ensureDoubleNewline()
	if c.mode == modeMarkdown {
		lang := ""
		if code := findChild(n, atom.Code); code != nil {
			cls := getAttr(code, "class")
			for _, part := range strings.Fields(cls) {
				if rest, ok := strings.CutPrefix(part, "language-"); ok {
					lang = rest
					break
				}
				if rest, ok := strings.CutPrefix(part, "lang-"); ok {
					lang = rest
					break
				}
			}
		}
		c.buf.WriteString("```")
		c.buf.WriteString(lang)
		c.buf.WriteByte('\n')
	}
	c.inPre = true
	c.walkChildren(n)
	c.inPre = false
	if c.mode == modeMarkdown {
		c.ensureNewline()
		c.buf.WriteString("```\n")
	} else {
		c.buf.WriteByte('\n')
	}
}

func (c *converter) handleCode(n *html.Node) {
	if c.inPre {
		c.walkChildren(n)
		return
	}
	if c.mode == modeMarkdown {
		c.buf.WriteByte('`')
		c.walkChildren(n)
		c.buf.WriteByte('`')
	} else {
		c.walkChildren(n)
	}
}

func (c *converter) handleBlockquote(n *html.Node) {
	c.ensureDoubleNewline()
	if c.mode == modeMarkdown {
		sub := &converter{mode: c.mode, inPre: c.inPre}
		sub.walkChildren(n)
		for i, line := range strings.Split(strings.TrimSpace(sub.buf.String()), "\n") {
			if i > 0 {
				c.buf.WriteByte('\n')
			}
			c.buf.WriteString("> ")
			c.buf.WriteString(line)
		}
		c.buf.WriteByte('\n')
	} else {
		c.walkChildren(n)
	}
}

func (c *converter) handleStrong(n *html.Node) {
	if c.mode == modeMarkdown {
		c.buf.WriteString("**")
		c.walkChildren(n)
		c.buf.WriteString("**")
	} else {
		c.walkChildren(n)
	}
}

func (c *converter) handleEmphasis(n *html.Node) {
	if c.mode == modeMarkdown {
		c.buf.WriteByte('*')
		c.walkChildren(n)
		c.buf.WriteByte('*')
	} else {
		c.walkChildren(n)
	}
}

func (c *converter) handleList(n *html.Node) {
	c.ensureNewline()
	c.listDepth++
	c.listType = append(c.listType, n.DataAtom)
	c.listIndex = append(c.listIndex, 0)
	c.walkChildren(n)
	c.listDepth--
	c.listType = c.listType[:len(c.listType)-1]
	c.listIndex = c.listIndex[:len(c.listIndex)-1]
	c.ensureNewline()
}

func (c *converter) handleListItem(n *html.Node) {
	c.ensureNewline()
	indent := strings.Repeat("  ", max(0, c.listDepth-1))
	c.buf.WriteString(indent)

	if len(c.listType) > 0 && c.listType[len(c.listType)-1] == atom.Ol {
		idx := len(c.listIndex) - 1
		c.listIndex[idx]++
		fmt.Fprintf(&c.buf, "%d. ", c.listIndex[idx])
	} else {
		c.buf.WriteString("- ")
	}
	c.walkChildren(n)
}

func (c *converter) handleTable(n *html.Node) {
	c.ensureDoubleNewline()
	rows := collectTableRows(n, c.mode)
	if len(rows) == 0 {
		return
	}
	colCount := 0
	for _, row := range rows {
		if len(row) > colCount {
			colCount = len(row)
		}
	}
	if c.mode == modeMarkdown {
		for i, row := range rows {
			c.buf.WriteByte('|')
			for j := 0; j < colCount; j++ {
				cell := ""
				if j < len(row) {
					cell = row[j]
				}
				c.buf.WriteByte(' ')
				c.buf.WriteString(cell)
				c.buf.WriteString(" |")
			}
			c.buf.WriteByte('\n')
			if i == 0 {
				c.buf.WriteByte('|')
				for j := 0; j < colCount; j++ {
					c.buf.WriteString(" --- |")
				}
				c.buf.WriteByte('\n')
			}
		}
	} else {
		for _, row := range rows {
			c.buf.WriteString(strings.Join(row, " | "))
			c.buf.WriteByte('\n')
		}
	}
	c.buf.WriteByte('\n')
}

func (c *converter) handleDefinitionTerm(n *html.Node) {
	c.ensureDoubleNewline()
	if c.mode == modeMarkdown {
		c.buf.WriteString("**")
		c.walkChildren(n)
		c.buf.WriteString("**")
	} else {
		c.walkChildren(n)
	}
	c.buf.WriteByte('\n')
}

func (c *converter) handleDefinitionDesc(n *html.Node) {
	c.ensureNewline()
	if c.mode == modeMarkdown {
		c.buf.WriteString(": ")
	}
	c.walkChildren(n)
	c.buf.WriteByte('\n')
}

// --- helpers ---

func getAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func findChild(n *html.Node, tag atom.Atom) *html.Node {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && c.DataAtom == tag {
			return c
		}
	}
	return nil
}

func findBody(doc *html.Node) *html.Node {
	var find func(*html.Node) *html.Node
	find = func(n *html.Node) *html.Node {
		if n.Type == html.ElementNode && n.DataAtom == atom.Body {
			return n
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if found := find(c); found != nil {
				return found
			}
		}
		return nil
	}
	if body := find(doc); body != nil {
		return body
	}
	return doc
}

func collapseWhitespace(s string) string {
	var buf strings.Builder
	buf.Grow(len(s))
	inSpace := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' {
			if !inSpace {
				buf.WriteByte(' ')
				inSpace = true
			}
		} else {
			buf.WriteRune(r)
			inSpace = false
		}
	}
	return buf.String()
}

func (c *converter) ensureNewline() {
	if c.buf.Len() == 0 {
		return
	}
	s := c.buf.String()
	if s[len(s)-1] != '\n' {
		c.buf.WriteByte('\n')
	}
}

func (c *converter) ensureDoubleNewline() {
	if c.buf.Len() == 0 {
		return
	}
	s := c.buf.String()
	if len(s) >= 2 && s[len(s)-1] == '\n' && s[len(s)-2] == '\n' {
		return
	}
	if s[len(s)-1] == '\n' {
		c.buf.WriteByte('\n')
	} else {
		c.buf.WriteString("\n\n")
	}
}

// collectTableRows extracts rows from a table node. Each row is a slice of cell strings.
func collectTableRows(table *html.Node, mode convertMode) [][]string {
	var rows [][]string
	var findRows func(*html.Node)
	findRows = func(n *html.Node) {
		if n.Type == html.ElementNode && n.DataAtom == atom.Tr {
			var cells []string
			for td := n.FirstChild; td != nil; td = td.NextSibling {
				if td.Type == html.ElementNode && (td.DataAtom == atom.Td || td.DataAtom == atom.Th) {
					sub := &converter{mode: mode}
					sub.walkChildren(td)
					cells = append(cells, strings.TrimSpace(sub.buf.String()))
				}
			}
			if len(cells) > 0 {
				rows = append(rows, cells)
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			findRows(c)
		}
	}
	findRows(table)
	return rows
}

// --- output cleanup ---

var reMultiNL = regexp.MustCompile(`\n{3,}`)

func cleanOutput(s string) string {
	s = reMultiNL.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

func cleanTextOutput(s string) string {
	lines := strings.Split(s, "\n")
	var clean []string
	for _, line := range lines {
		line = strings.TrimRight(line, " \t")
		clean = append(clean, line)
	}
	s = strings.Join(clean, "\n")
	s = reMultiNL.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// stripTagsFallback is a last-resort fallback if the HTML parser fails.
var reStripTags = regexp.MustCompile(`<[^>]+>`)

func stripTagsFallback(s string) string {
	return strings.TrimSpace(reStripTags.ReplaceAllString(s, ""))
}

// markdownToText strips markdown formatting for text mode.
func markdownToText(md string) string {
	s := md
	s = regexp.MustCompile(`(?m)^#{1,6}\s+`).ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "**", "")
	s = strings.ReplaceAll(s, "__", "")
	s = regexp.MustCompile("`[^`]+`").ReplaceAllStringFunc(s, func(m string) string {
		return strings.Trim(m, "`")
	})
	s = regexp.MustCompile(`\[([^\]]+)\]\([^)]+\)`).ReplaceAllString(s, "$1")
	s = regexp.MustCompile(`!\[([^\]]*)\]\([^)]+\)`).ReplaceAllString(s, "$1")
	s = reMultiNL.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
