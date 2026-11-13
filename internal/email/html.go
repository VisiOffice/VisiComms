package email

import (
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

// hiddenStyle matches inline styles that hide text from a reader but not
// from a model reading the HTML.
var hiddenStyle = regexp.MustCompile(`(?i)display\s*:\s*none|visibility\s*:\s*hidden|font-size\s*:\s*0(?:\D|$)|opacity\s*:\s*0(?:\D|$)|max-height\s*:\s*0(?:\D|$)`)

// HTMLText is the text a reader sees, whitespace collapsed.
func HTMLText(src string) string {
	visible, _ := SplitHidden(src)
	return visible
}

// SplitHidden returns the visible text and the hidden text of an HTML body.
// Script and style contents are dropped from both.
func SplitHidden(src string) (visible, hidden string) {
	if src == "" {
		return "", ""
	}
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		return "", ""
	}
	var vis, hid []string
	var walk func(n *html.Node, isHidden bool)
	walk = func(n *html.Node, isHidden bool) {
		if n.Type == html.ElementNode {
			if n.Data == "script" || n.Data == "style" {
				return
			}
			for _, a := range n.Attr {
				if a.Key == "hidden" || (a.Key == "style" && hiddenStyle.MatchString(a.Val)) {
					isHidden = true
				}
			}
		}
		if n.Type == html.TextNode {
			if t := strings.TrimSpace(n.Data); t != "" {
				if isHidden {
					hid = append(hid, t)
				} else {
					vis = append(vis, t)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, isHidden)
		}
	}
	walk(doc, false)
	return squish(strings.Join(vis, " ")), squish(strings.Join(hid, " "))
}

func squish(s string) string { return strings.Join(strings.Fields(s), " ") }
