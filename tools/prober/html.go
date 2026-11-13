package main

import (
	"bytes"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

// Online Boutique ids look like OLJCESPC7Z; allow a little more in case the catalog changes.
var productIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// productIDs returns the ids of the product pages the page links to, in page order, without
// duplicates. The home page links every product as <a href="/product/<id>">.
func productIDs(page []byte) []string {
	z := html.NewTokenizer(bytes.NewReader(page))
	var ids []string
	seen := map[string]bool{}
	for {
		switch z.Next() {
		case html.ErrorToken:
			return ids
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			if string(name) != "a" || !hasAttr {
				continue
			}
			if id := productIDFromLink(attrs(z)["href"]); id != "" && !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
}

func productIDFromLink(href string) string {
	u, err := url.Parse(strings.TrimSpace(href))
	if err != nil {
		return ""
	}
	_, id, found := strings.Cut(u.Path, "/product/")
	if !found || !productIDPattern.MatchString(id) {
		return ""
	}
	return id
}

type option struct {
	value    string
	hasValue bool
	text     string
	selected bool
}

// findForm returns the fields a browser would submit with the first form whose action path ends
// with actionSuffix (which starts with "/"): named inputs (checkboxes and radio buttons only when
// checked, no buttons), the selected (or else the first) option of each select, and textareas.
func findForm(page []byte, actionSuffix string) (url.Values, bool) {
	z := html.NewTokenizer(bytes.NewReader(page))
	var (
		inForm   bool
		values   = url.Values{}
		selName  string
		inSelect bool
		options  []option
		textName string
		inText   bool
		text     strings.Builder
	)
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			return nil, false
		case html.StartTagToken, html.SelfClosingTagToken:
			name, _ := z.TagName()
			a := attrs(z)
			switch string(name) {
			case "form":
				if !inForm && actionMatches(a["action"], actionSuffix) {
					inForm = true
				}
			case "input":
				if inForm {
					addInput(values, a)
				}
			case "select":
				if inForm {
					_, disabled := a["disabled"]
					selName, inSelect, options = a["name"], !disabled, nil
				}
			case "option":
				if inForm && inSelect {
					value, hasValue := a["value"]
					_, selected := a["selected"]
					options = append(options, option{value: value, hasValue: hasValue, selected: selected})
				}
			case "textarea":
				if inForm && tt == html.StartTagToken {
					textName, inText = a["name"], true
					text.Reset()
				}
			}
		case html.TextToken:
			switch {
			case inText:
				text.Write(z.Text())
			case inSelect && len(options) > 0:
				options[len(options)-1].text += string(z.Text())
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			switch string(name) {
			case "select":
				if inSelect && selName != "" {
					if v, ok := chosenOption(options); ok {
						values.Add(selName, v)
					}
				}
				inSelect = false
			case "textarea":
				if inText && textName != "" {
					values.Add(textName, text.String())
				}
				inText = false
			case "form":
				if inForm {
					return values, true
				}
			}
		}
	}
}

func addInput(values url.Values, a map[string]string) {
	name := a["name"]
	if _, disabled := a["disabled"]; name == "" || disabled {
		return
	}
	switch strings.ToLower(a["type"]) {
	case "submit", "button", "image", "reset", "file":
		return
	case "checkbox", "radio":
		if _, checked := a["checked"]; !checked {
			return
		}
		if _, ok := a["value"]; !ok {
			values.Add(name, "on")
			return
		}
	}
	values.Add(name, a["value"])
}

// chosenOption is what a single-choice select submits: the last selected option, or else the first.
func chosenOption(options []option) (string, bool) {
	if len(options) == 0 {
		return "", false
	}
	chosen := options[0]
	for _, o := range options {
		if o.selected {
			chosen = o
		}
	}
	if chosen.hasValue {
		return chosen.value, true
	}
	return strings.Join(strings.Fields(chosen.text), " "), true
}

func actionMatches(action, suffix string) bool {
	u, err := url.Parse(strings.TrimSpace(action))
	return err == nil && strings.HasSuffix(u.Path, suffix)
}

// preText returns the text of the first <pre> element: where the frontend's error page
// (templates/error.html) prints the error.
func preText(page []byte) string {
	z := html.NewTokenizer(bytes.NewReader(page))
	inPre := false
	var b strings.Builder
	for {
		switch z.Next() {
		case html.ErrorToken:
			return b.String()
		case html.StartTagToken:
			if name, _ := z.TagName(); string(name) == "pre" {
				inPre = true
			}
		case html.TextToken:
			if inPre {
				b.Write(z.Text())
			}
		case html.EndTagToken:
			if name, _ := z.TagName(); string(name) == "pre" && inPre {
				return b.String()
			}
		}
	}
}

// attrs returns the current tag's attributes; the tokenizer lower-cases the keys.
func attrs(z *html.Tokenizer) map[string]string {
	a := map[string]string{}
	for {
		key, val, more := z.TagAttr()
		if len(key) > 0 {
			a[string(key)] = string(val)
		}
		if !more {
			return a
		}
	}
}
