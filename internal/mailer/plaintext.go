package mailer

import (
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// The free-form template's body is the one variable that carries markup, and it
// goes into both bodies of the same mail: html/template renders it as markup,
// text/template would print the tags as they are. A recipient reading the
// plain-text alternative would see `<p>…</p>`.
//
// So the text renderer gets its own safeHTML, which turns that markup into
// something worth reading — the same shape html-to-text gives the rest of the
// mail, where a link reads as its text followed by its address.
func plainTextFromHTML(markup string) string {
	doc, err := html.Parse(strings.NewReader(markup))
	if err != nil {
		// Parsing barely ever fails — it recovers from bad markup — but text with
		// the tags in it still beats an empty body.
		return markup
	}

	var out strings.Builder
	walkForText(doc, &out, 0)

	return strings.TrimSpace(collapseBlankLines(out.String()))
}

// listState carries the numbering an <ol> needs down to its items.
type listState struct {
	ordered bool
	index   int
}

func walkForText(node *html.Node, out *strings.Builder, depth int) {
	var list *listState

	for child := node.FirstChild; child != nil; child = child.NextSibling {
		switch child.Type {
		case html.TextNode:
			out.WriteString(strings.ReplaceAll(child.Data, "\n", " "))

		case html.ElementNode:
			switch child.Data {
			case "br":
				out.WriteString("\n")
			case "p", "h2", "h3", "blockquote", "div":
				out.WriteString("\n\n")
				walkForText(child, out, depth)
				out.WriteString("\n\n")
			case "ul", "ol":
				list = &listState{ordered: child.Data == "ol"}
				out.WriteString("\n")
				walkListForText(child, out, list, depth)
				out.WriteString("\n")
			case "a":
				// html-to-text writes a link as its text and then its address, and
				// the rest of the plain-text part already reads that way.
				var inner strings.Builder
				walkForText(child, &inner, depth)
				text := strings.TrimSpace(inner.String())
				href := attr(child, "href")
				switch {
				case href == "" || href == text:
					out.WriteString(text)
				case text == "":
					out.WriteString(href)
				default:
					out.WriteString(text + " " + href)
				}
			default:
				walkForText(child, out, depth)
			}
		}
	}
}

func walkListForText(node *html.Node, out *strings.Builder, list *listState, depth int) {
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if child.Type != html.ElementNode || child.Data != "li" {
			continue
		}

		var inner strings.Builder
		walkForText(child, &inner, depth+1)

		marker := "- "
		if list.ordered {
			list.index++
			marker = strconv.Itoa(list.index) + ". "
		}
		out.WriteString("\n" + marker + strings.TrimSpace(collapseBlankLines(inner.String())))
	}
}

func attr(node *html.Node, name string) string {
	for _, a := range node.Attr {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}

// collapseBlankLines keeps paragraphs apart by exactly one blank line, however
// many the tags produced.
func collapseBlankLines(text string) string {
	lines := strings.Split(text, "\n")
	var kept []string
	blank := 0
	for _, line := range lines {
		line = strings.TrimRight(line, " \t")
		if strings.TrimSpace(line) == "" {
			blank++
			if blank > 1 || len(kept) == 0 {
				continue
			}
			kept = append(kept, "")
			continue
		}
		blank = 0
		kept = append(kept, strings.Join(strings.Fields(line), " "))
	}
	return strings.Join(kept, "\n")
}

func textValue(v any) string {
	switch value := v.(type) {
	case nil:
		return ""
	case string:
		return value
	default:
		return fmt.Sprint(value)
	}
}
