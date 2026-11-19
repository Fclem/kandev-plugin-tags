package main

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kdlbs/kandev/pluginsdk"
)

type sidebarGroupingTag struct {
	Name  string
	Color string
}

func sidebarTagDefinition(tag sharedTag) (sidebarGroupingTag, error) {
	if !sidebarTagText(tag.ID) || strings.TrimFunc(tag.Name, stripFromEdges) == "" || len(tag.Name) > 256 || !utf8.ValidString(tag.Name) {
		return sidebarGroupingTag{}, errSidebarGroupingState
	}
	color, err := normalizeTagColor(tag.Color)
	if err != nil {
		return sidebarGroupingTag{}, errSidebarGroupingState
	}
	return sidebarGroupingTag{Name: tag.Name, Color: color}, nil
}

func sidebarTagValue(tag sharedTag) (pluginsdk.SidebarGroupingValue, error) {
	definition, err := sidebarTagDefinition(tag)
	if err != nil {
		return pluginsdk.SidebarGroupingValue{}, err
	}
	label := definition.Name
	if strings.ContainsRune(label, '\\') || strings.IndexFunc(label, unicode.IsControl) >= 0 {
		var escaped strings.Builder
		escaped.Grow(len(label))
		writeSidebarTagName(&escaped, label, false)
		label = escaped.String()
	}
	if !sidebarTagText(label) {
		return pluginsdk.SidebarGroupingValue{}, errSidebarGroupingState
	}
	return pluginsdk.SidebarGroupingValue{ID: tag.ID, Label: label, Color: definition.Color}, nil
}

// Names are escaped directly from their raw spelling. Quoted mode emits a JSON
// string literal, including control characters outside JSON's ASCII range.
func writeSidebarTagName(out *strings.Builder, name string, quoted bool) {
	if quoted {
		out.WriteByte('"')
	}
	for _, r := range name {
		switch r {
		case '\\':
			out.WriteString(`\\`)
		case '"':
			if quoted {
				out.WriteString(`\"`)
			} else {
				out.WriteByte('"')
			}
		case '\b':
			out.WriteString(`\b`)
		case '\t':
			out.WriteString(`\t`)
		case '\n':
			out.WriteString(`\n`)
		case '\f':
			out.WriteString(`\f`)
		case '\r':
			out.WriteString(`\r`)
		default:
			if unicode.IsControl(r) {
				out.WriteString(`\u`)
				for shift := 12; shift >= 0; shift -= 4 {
					out.WriteByte("0123456789abcdef"[(r>>shift)&15])
				}
			} else {
				out.WriteRune(r)
			}
		}
	}
	if quoted {
		out.WriteByte('"')
	}
}
