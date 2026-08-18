package telegram

import (
	"errors"
	"html"
	"regexp"
	"strconv"
	"strings"
)

var (
	codeSpanPattern = regexp.MustCompile("`([^`\\n]+)`")
	linkPattern     = regexp.MustCompile("\\[([^\\]]+)\\]\\(([^)]+)\\)")
	boldPattern     = regexp.MustCompile("\\*\\*([^*\\n]+)\\*\\*|__([^_\\n]+)__")
	italicPattern   = regexp.MustCompile("(^|[^*])\\*([^*\\n]+)\\*|(^|[^_])_([^_\\n]+)_")
	spoilerPattern  = regexp.MustCompile("\\|\\|([^|\\n]+)\\|\\|")
	headingPattern  = regexp.MustCompile("^#{1,6}\\s+(.+)$")
	bulletPattern   = regexp.MustCompile("^[-*]\\s+(.+)$")
	orderedPattern  = regexp.MustCompile("^([0-9]+)\\.\\s+(.+)$")
	quotePattern    = regexp.MustCompile("^>\\s*(.*)$")
)

func RenderMarkdown(input string) (string, error) {
	if input == "" {
		return "", nil
	}
	lines := strings.Split(strings.ReplaceAll(input, "\r\n", "\n"), "\n")
	var output strings.Builder
	inCodeBlock := false
	for index := 0; index < len(lines); index++ {
		line := lines[index]
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if inCodeBlock {
				output.WriteString("</pre>")
			} else {
				output.WriteString("<pre>")
			}
			inCodeBlock = !inCodeBlock
			continue
		}
		if inCodeBlock {
			output.WriteString(escapeHTML(line))
			if index+1 < len(lines) {
				output.WriteByte('\n')
			}
			continue
		}
		if trimmed == "" {
			output.WriteByte('\n')
			continue
		}
		if matches := headingPattern.FindStringSubmatch(trimmed); matches != nil {
			output.WriteString("<b>")
			output.WriteString(renderInline(matches[1]))
			output.WriteString("</b>")
		} else if matches := bulletPattern.FindStringSubmatch(trimmed); matches != nil {
			output.WriteString("• ")
			output.WriteString(renderInline(matches[1]))
		} else if matches := orderedPattern.FindStringSubmatch(trimmed); matches != nil {
			output.WriteString(matches[1] + ". " + renderInline(matches[2]))
		} else if matches := quotePattern.FindStringSubmatch(trimmed); matches != nil {
			output.WriteString("<blockquote>")
			output.WriteString(renderInline(matches[1]))
			output.WriteString("</blockquote>")
		} else if trimmed == "---" || trimmed == "***" {
			output.WriteString("────────")
		} else if strings.Count(trimmed, "|") >= 2 && index+1 < len(lines) && isTableSeparator(strings.TrimSpace(lines[index+1])) {
			output.WriteString(renderTable(trimmed))
			index++
		} else {
			output.WriteString(renderInline(trimmed))
		}
		if index+1 < len(lines) {
			output.WriteByte('\n')
		}
	}
	if inCodeBlock {
		output.WriteString("</pre>")
	}
	result := output.String()
	if result == "" {
		return "", nil
	}
	return result, nil
}

func renderInline(input string) string {
	placeholders := make([]string, 0)
	protect := func(value string) string {
		placeholder := "\x00PROTECTED" + strconv.Itoa(len(placeholders)) + "\x00"
		placeholders = append(placeholders, value)
		return placeholder
	}
	value := input
	value = codeSpanPattern.ReplaceAllStringFunc(value, func(match string) string {
		parts := codeSpanPattern.FindStringSubmatch(match)
		return protect("<code>" + escapeHTML(parts[1]) + "</code>")
	})
	value = spoilerPattern.ReplaceAllStringFunc(value, func(match string) string {
		parts := spoilerPattern.FindStringSubmatch(match)
		return protect("<span class=\"tg-spoiler\">" + renderInline(parts[1]) + "</span>")
	})
	value = linkPattern.ReplaceAllStringFunc(value, func(match string) string {
		parts := linkPattern.FindStringSubmatch(match)
		if !safeLink(parts[2]) {
			return match
		}
		return protect("<a href=\"" + html.EscapeString(parts[2]) + "\">" + renderInline(parts[1]) + "</a>")
	})
	value = boldPattern.ReplaceAllStringFunc(value, func(match string) string {
		parts := boldPattern.FindStringSubmatch(match)
		text := parts[1]
		if text == "" {
			text = parts[2]
		}
		return protect("<b>" + renderInline(text) + "</b>")
	})
	value = italicPattern.ReplaceAllStringFunc(value, func(match string) string {
		parts := italicPattern.FindStringSubmatch(match)
		text := parts[2]
		if text == "" {
			text = parts[4]
		}
		prefix := parts[1]
		if prefix == "" {
			prefix = parts[3]
		}
		return protect(prefix + "<i>" + renderInline(text) + "</i>")
	})
	value = escapeHTML(value)
	for index, placeholderValue := range placeholders {
		value = strings.ReplaceAll(value, "\x00PROTECTED"+strconv.Itoa(index)+"\x00", placeholderValue)
	}
	return value
}

func isTableSeparator(line string) bool {
	parts := strings.Split(line, "|")
	if len(parts) < 2 {
		return false
	}
	start := 0
	end := len(parts)
	if len(parts) > 0 && strings.TrimSpace(parts[0]) == "" {
		start = 1
	}
	if end > start && strings.TrimSpace(parts[end-1]) == "" {
		end--
	}
	if end-start < 1 {
		return false
	}
	for index := start; index < end; index++ {
		trimmed := strings.TrimSpace(parts[index])
		trimmed = strings.Trim(trimmed, ":")
		if trimmed == "" {
			return false
		}
		if strings.Trim(trimmed, "-") != "" {
			return false
		}
	}
	return true
}

func renderTable(header string) string {
	cells := strings.Split(header, "|")
	var result strings.Builder
	for index, cell := range cells {
		if index > 0 {
			result.WriteString("  ")
		}
		result.WriteString(renderInline(strings.TrimSpace(cell)))
	}
	return result.String()
}

func safeLink(value string) bool {
	return strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "tg://")
}

func escapeHTML(value string) string {
	return html.EscapeString(value)
}

func splitTelegramText(rendered string, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, errors.New("telegram: invalid message limit")
	}
	if len(rendered) <= limit {
		return []string{rendered}, nil
	}
	parts := strings.Split(rendered, "\n")
	var chunks []string
	current := ""
	for _, part := range parts {
		candidate := part
		if current != "" {
			candidate = current + "\n" + part
		}
		if len(candidate) <= limit {
			current = candidate
			continue
		}
		if current != "" {
			chunks = append(chunks, current)
		}
		if len(part) > limit {
			for len(part) > limit {
				chunks = append(chunks, part[:limit])
				part = part[limit:]
			}
			current = part
		} else {
			current = part
		}
	}
	if current != "" {
		chunks = append(chunks, current)
	}
	return chunks, nil
}
