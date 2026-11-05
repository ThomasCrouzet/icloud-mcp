package mail

import (
	"html"
	"strings"
)

const (
	maxHTMLNodes    = 100_000
	maxHTMLDepth    = 1_000
	maxHTMLTagBytes = 4_096
)

var errMalformedHTML = &Error{Code: CodeProtocolError, Message: "HTML body is malformed"}

// htmlToText converts bounded HTML to plain text without a browser or network
// access. Script, style, template, head, and comment content is excluded.
// Block elements become newlines. The result is truncated at max bytes.
func htmlToText(input string, maxBytes int) (string, bool, error) {
	var out strings.Builder
	index, nodes, depth := 0, 0, 0
	for index < len(input) {
		if input[index] == '<' {
			if strings.HasPrefix(input[index:], "<!--") {
				end := strings.Index(input[index+4:], "-->")
				if end < 0 {
					return "", false, errMalformedHTML
				}
				index += 4 + end + 3
				continue
			}
			end := htmlTagEnd(input, index)
			if end < 0 {
				return "", false, errMalformedHTML
			}
			nodes++
			if nodes > maxHTMLNodes {
				return "", true, nil
			}
			raw := input[index+1 : end]
			closing := strings.HasPrefix(raw, "/")
			selfClosing := strings.HasSuffix(strings.TrimSpace(raw), "/")
			name := htmlTagName(raw)
			lower := strings.ToLower(name)
			if len(lower) > maxHTMLTagBytes {
				return "", false, errMalformedHTML
			}
			switch lower {
			case "script", "style", "template", "head":
				if !closing && !selfClosing {
					lowerInput := strings.ToLower(input[end+1:])
					match := strings.Index(lowerInput, "</"+lower)
					if match < 0 {
						return "", false, errMalformedHTML
					}
					closeEnd := htmlTagEnd(input, end+1+match)
					if closeEnd < 0 {
						return "", false, errMalformedHTML
					}
					index = closeEnd + 1
					continue
				}
			case "br", "p", "div", "li", "tr", "h1", "h2", "h3", "h4", "h5", "h6", "blockquote", "pre", "table", "ul", "ol":
				appendHTMLNewline(&out)
			}
			if !closing && !selfClosing && !isHTMLVoid(lower) {
				depth++
				if depth > maxHTMLDepth {
					return "", false, errMalformedHTML
				}
			} else if closing && depth > 0 {
				depth--
			}
			index = end + 1
			continue
		}
		next := strings.IndexByte(input[index:], '<')
		if next < 0 {
			next = len(input) - index
		}
		text := html.UnescapeString(input[index : index+next])
		out.WriteString(text)
		index += next
		if out.Len() > maxBytes*4 {
			break
		}
	}
	normalized := normalizeHTMLText(out.String())
	if len(normalized) > maxBytes {
		return truncateUTF8(normalized, maxBytes), true, nil
	}
	return normalized, false, nil
}

func htmlTagEnd(input string, start int) int {
	quoted := byte(0)
	for index := start + 1; index < len(input); index++ {
		if input[index] == '\'' || input[index] == '"' {
			if quoted == 0 {
				quoted = input[index]
			} else if quoted == input[index] {
				quoted = 0
			}
			continue
		}
		if input[index] == '>' && quoted == 0 {
			return index
		}
	}
	return -1
}

func htmlTagName(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "/")
	end := strings.IndexAny(raw, " \t\r\n/>")
	if end < 0 {
		return raw
	}
	return raw[:end]
}

func isHTMLVoid(name string) bool {
	switch name {
	case "area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta", "param", "source", "track", "wbr":
		return true
	default:
		return false
	}
}

func appendHTMLNewline(out *strings.Builder) {
	value := out.String()
	if value != "" && !strings.HasSuffix(value, "\n") {
		out.WriteByte('\n')
	}
}

func normalizeHTMLText(value string) string {
	var out strings.Builder
	space := false
	newlines := 0
	for _, character := range value {
		if character == '\r' || character == '\t' || character == ' ' {
			space = true
			continue
		}
		if character == '\n' {
			space = false
			if newlines < 2 && out.Len() > 0 {
				out.WriteByte('\n')
			}
			if newlines < 2 {
				newlines++
			}
			continue
		}
		if space && out.Len() > 0 {
			out.WriteByte(' ')
		}
		space = false
		newlines = 0
		out.WriteRune(character)
	}
	return strings.TrimSpace(out.String())
}
