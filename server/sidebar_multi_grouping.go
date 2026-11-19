package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"

	"github.com/kdlbs/kandev/pluginsdk"
)

// Multi-tag files each task under every nonempty subset of its distinct
// effective shared tags. Growth is exponential, so each task's subset count is
// checked against the remaining budgets before any subset is enumerated, and an
// overflow fails the whole snapshot instead of returning partial combinations.
func sidebarMultiTagSnapshot(ctx context.Context, doc tagDoc) (*pluginsdk.SidebarGroupingSnapshot, error) {
	catalog := make(map[string]pluginsdk.SidebarGroupingValue, len(doc.Tags))
	for _, tag := range doc.Tags {
		value, err := sidebarTagValue(tag)
		if err != nil {
			return nil, err
		}
		if _, duplicate := catalog[tag.ID]; duplicate {
			return nil, errSidebarGroupingState
		}
		catalog[tag.ID] = value
	}
	groups := []pluginsdk.SidebarGroupingValue{}
	members := []pluginsdk.SidebarGroupingMembership{}
	values := map[string]struct{}{}
	var tagIDs, subset []string
	for taskID, entries := range doc.Tasks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		tagIDs = tagIDs[:0]
		for _, entry := range entries {
			if !entry.Agent && !entry.Human {
				continue
			}
			if !sidebarTagText(taskID) || !sidebarTagText(entry.TagID) {
				return nil, errSidebarGroupingState
			}
			if _, exists := catalog[entry.TagID]; exists {
				tagIDs = append(tagIDs, entry.TagID)
			}
		}
		slices.Sort(tagIDs)
		tagIDs = slices.Compact(tagIDs)
		// One task's subsets are pairwise distinct values, so they must fit both
		// the value budget and the memberships still available.
		subsets, ok := sidebarSubsetCount(len(tagIDs), min(pluginsdk.MaxSidebarGroupingValues, pluginsdk.MaxSidebarGroupingMemberships-len(members)))
		if !ok {
			return nil, errSidebarGroupingState
		}
		for mask := 1; mask <= subsets; mask++ {
			subset = subset[:0]
			for bit, tagID := range tagIDs {
				if mask&(1<<bit) != 0 {
					subset = append(subset, tagID)
				}
			}
			id, err := sidebarSetID(subset)
			if err != nil {
				return nil, err
			}
			if _, exists := values[id]; !exists {
				if len(groups) == pluginsdk.MaxSidebarGroupingValues {
					return nil, errSidebarGroupingState
				}
				value, ok := sidebarSetValue(id, subset, catalog)
				if !ok {
					return nil, errSidebarGroupingState
				}
				values[id] = struct{}{}
				groups = append(groups, value)
			}
			members = append(members, pluginsdk.SidebarGroupingMembership{TaskID: taskID, GroupID: id})
		}
	}
	return completeSidebarSnapshot(groups, members), nil
}

// sidebarSubsetCount returns 2^tags-1 by bounded doubling, failing as soon as
// the count exceeds limit so neither a shift nor the power set can overflow.
func sidebarSubsetCount(tags, limit int) (int, bool) {
	count := 0
	for range tags {
		count = 2*count + 1
		if count > limit {
			return 0, false
		}
	}
	return count, true
}

// sidebarSetID derives a combination identity from its sorted member tag IDs
// alone: names and colors are mutable, and JSON keeps IDs containing
// punctuation distinct from any delimiter-joined combination.
func sidebarSetID(sortedTagIDs []string) (string, error) {
	encoded, err := json.Marshal(sortedTagIDs)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return "set-" + hex.EncodeToString(sum[:]), nil
}

// Catalog names contain no control characters, so escaping backslash and quote
// yields a valid JSON string literal without HTML escaping of <, > or &.
var sidebarLabelQuote = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

// sidebarSetValue labels a combination as its quoted member names joined by
// " + " in canonical ID order. Quoting keeps a "+" inside one name unambiguous.
// Only a singleton keeps its tag color; a compound value has no single owner.
// A label beyond the transport bound is refused rather than shortened.
func sidebarSetValue(id string, sortedTagIDs []string, catalog map[string]pluginsdk.SidebarGroupingValue) (pluginsdk.SidebarGroupingValue, bool) {
	var label strings.Builder
	for i, tagID := range sortedTagIDs {
		if i > 0 {
			label.WriteString(" + ")
		}
		label.WriteByte('"')
		_, _ = sidebarLabelQuote.WriteString(&label, catalog[tagID].Label)
		label.WriteByte('"')
	}
	value := pluginsdk.SidebarGroupingValue{ID: id, Label: label.String()}
	if len(sortedTagIDs) == 1 {
		value.Color = catalog[sortedTagIDs[0]].Color
	}
	return value, sidebarTagText(value.Label)
}
