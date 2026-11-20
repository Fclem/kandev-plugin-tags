package main

import "strings"

// Classification is decoded only when its field spelling matches the preflight.
func sidebarFieldCase(fields map[string]any, canonical ...string) bool {
	for key := range fields {
		for _, name := range canonical {
			if key != name && strings.EqualFold(key, name) {
				return false
			}
		}
	}
	return true
}

func sidebarCatalogFieldCase(value any) bool {
	entries, ok := value.([]any)
	if !ok {
		return false
	}
	for _, entry := range entries {
		fields, ok := entry.(map[string]any)
		if !ok || !sidebarFieldCase(fields, "id", "name", "color") {
			return false
		}
	}
	return true
}
