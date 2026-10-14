package tracking

import (
	"strings"

	"golang.org/x/net/html"
)

// PixelURL is a recipient's pixel: <base>/t/o/<token>. No .gif: a CDN
// caches by extension.
func PixelURL(baseURL string, recipientToken []byte) string {
	return baseURL + "/t/o/" + EncodeToken(recipientToken)
}

// ClickURL is a recipient's click on one link: <base>/t/c/<token>/<link>.
func ClickURL(baseURL string, recipientToken, linkToken []byte) string {
	return baseURL + "/t/c/" + EncodeToken(recipientToken) + "/" + EncodeToken(linkToken)
}

// PixelHTML is the pixel as it goes into a mail: a 1×1 image with an empty
// alt, so a client that does not load it shows nothing.
func PixelHTML(pixelURL string) string {
	return `<img src="` + html.EscapeString(pixelURL) +
		`" width="1" height="1" alt="" style="display:block;width:1px;height:1px;border:0;margin:0;padding:0" />`
}

// Decorate is a tracked mail as it goes out: body with the pixel right after
// the <body> tag (first of all when there is none: Gmail clips the end of a
// long mail, never its start), and each <a> whose href is a key of clicks
// pointing at its value instead. An <a> marked data-skymail-notrack keeps its
// href. Nothing else changes: every other byte of body is copied as it is,
// so markup the tokenizer cannot read, conditional comments, styles and
// text that looks like a link come out as they went in. No sentence, footer
// or link is added (T8).
func Decorate(body, pixelURL string, clicks map[string]string) string {
	img := PixelHTML(pixelURL)
	var out strings.Builder
	out.Grow(len(body) + len(img) + 64*len(clicks))
	z := html.NewTokenizer(strings.NewReader(body))
	consumed, placed := 0, false
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		// The raw bytes first: reading the tag's name and attributes
		// lower-cases and unescapes them in the tokenizer's buffer.
		raw := string(z.Raw())
		consumed += len(raw)
		if tt == html.StartTagToken || tt == html.SelfClosingTagToken {
			name, hasAttr := z.TagName()
			switch {
			case string(name) == "body":
				out.WriteString(raw)
				if !placed {
					out.WriteString(img)
					placed = true
				}
				continue
			case string(name) == "a" && hasAttr && len(clicks) > 0:
				if tag, ok := rewriteAnchor(z, tt == html.SelfClosingTagToken, clicks); ok {
					out.WriteString(tag)
					continue
				}
			}
		}
		out.WriteString(raw)
	}
	// What the tokenizer did not hand back (a tag cut off at the end).
	out.WriteString(body[consumed:])
	if !placed {
		return img + out.String()
	}
	return out.String()
}

// rewriteAnchor writes the current <a> anew with its href pointing at its
// click address, when its href is a key of clicks and it is not marked
// data-skymail-notrack. Its other attributes keep their order and values.
func rewriteAnchor(z *html.Tokenizer, selfClosing bool, clicks map[string]string) (string, bool) {
	type attribute struct{ key, value string }
	var attributes []attribute
	href := -1
	for more := true; more; {
		var key, value []byte
		key, value, more = z.TagAttr()
		switch string(key) {
		case NoTrackAttribute:
			return "", false
		case "href":
			if href < 0 {
				href = len(attributes)
			}
		}
		attributes = append(attributes, attribute{string(key), string(value)})
	}
	if href < 0 {
		return "", false
	}
	click, ok := clicks[strings.TrimSpace(attributes[href].value)]
	if !ok {
		return "", false
	}
	attributes[href].value = click

	var tag strings.Builder
	tag.WriteString("<a")
	for _, a := range attributes {
		tag.WriteString(" ")
		tag.WriteString(a.key)
		tag.WriteString(`="`)
		tag.WriteString(html.EscapeString(a.value))
		tag.WriteString(`"`)
	}
	if selfClosing {
		tag.WriteString("/>")
	} else {
		tag.WriteString(">")
	}
	return tag.String(), true
}
