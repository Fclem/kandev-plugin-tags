package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kdlbs/kandev/pluginsdk"
)

var errSidebarGroupingState = errors.New("shared tag state is invalid or exceeds complete grouping limits")

// sidebarGroupingAction serves one declared grouping. Every builder consumes the
// same strictly validated shared document and the same complete-response bound.
func (p *tagsPlugin) sidebarGroupingAction(ctx context.Context, req *pluginsdk.PluginActionRequest, build func(context.Context, tagDoc) (*pluginsdk.SidebarGroupingSnapshot, error)) (*pluginsdk.PluginActionResponse, error) {
	if err := requireActionWorkspace(req); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	doc, err := p.readSidebarTagDoc(ctx, req.Context.WorkspaceID)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	snapshot, err := build(ctx, doc)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	response, err := jsonAction(snapshot)
	if err != nil {
		return nil, err
	}
	if len(response.Body) > pluginsdk.MaxSidebarGroupingResponseBytes {
		return nil, errSidebarGroupingState
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return response, nil
}

// Complete classification requires a recognized whole shared document. The older
// board reader retains its permissive legacy-display behavior independently.
func (p *tagsPlugin) readSidebarTagDoc(ctx context.Context, workspace string) (tagDoc, error) {
	host := p.Host()
	if host == nil {
		return tagDoc{}, errSidebarGroupingState
	}
	raw, found, err := host.GetState(ctx, "workspace", workspace, tagStateKey)
	if err != nil {
		return tagDoc{}, err
	}
	if !found {
		return newTagDoc(), nil
	}
	version, ok := raw["version"].(float64)
	if !ok || (version != 1 && version != 2) {
		return tagDoc{}, errSidebarGroupingState
	}
	applications, ok := raw["tasks"].(map[string]any)
	if !ok {
		return tagDoc{}, errSidebarGroupingState
	}
	if version == 2 {
		if _, ok := raw["tags"].([]any); !ok {
			return tagDoc{}, errSidebarGroupingState
		}
	} else {
		if _, present := raw["tags"]; present {
			return tagDoc{}, errSidebarGroupingState
		}
	}
	if err := validateSidebarApplications(applications, version == 1); err != nil {
		return tagDoc{}, err
	}
	return decodeTagDoc(raw)
}

func validateSidebarApplications(applications map[string]any, legacy bool) error {
	identityKey := "tag_id"
	if legacy {
		identityKey = "tag"
	}
	rawCount := 0
	for _, value := range applications {
		entries, ok := value.([]any)
		if !ok || len(entries) > pluginsdk.MaxSidebarGroupingMemberships-rawCount {
			return errSidebarGroupingState
		}
		rawCount += len(entries)
		for _, entry := range entries {
			fields, ok := entry.(map[string]any)
			if !ok {
				return errSidebarGroupingState
			}
			identity, ok := fields[identityKey].(string)
			if !ok || !sidebarTagText(identity) {
				return errSidebarGroupingState
			}
			if legacy {
				continue
			}
			for _, key := range []string{"agent", "human"} {
				if flag, present := fields[key]; present {
					if _, ok := flag.(bool); !ok {
						return errSidebarGroupingState
					}
				}
			}
		}
	}
	return nil
}

func sidebarTagSnapshot(_ context.Context, doc tagDoc) (*pluginsdk.SidebarGroupingSnapshot, error) {
	if len(doc.Tags) > pluginsdk.MaxSidebarGroupingValues {
		return nil, errSidebarGroupingState
	}
	groups := make([]pluginsdk.SidebarGroupingValue, len(doc.Tags))
	known := make(map[string]struct{}, len(doc.Tags))
	for i, tag := range doc.Tags {
		value, err := sidebarTagValue(tag)
		if err != nil {
			return nil, err
		}
		if _, duplicate := known[tag.ID]; duplicate {
			return nil, errSidebarGroupingState
		}
		groups[i] = value
		known[tag.ID] = struct{}{}
	}
	pairs := make(map[pluginsdk.SidebarGroupingMembership]struct{}, min(len(doc.Tasks), pluginsdk.MaxSidebarGroupingMemberships))
	for taskID, entries := range doc.Tasks {
		for _, entry := range entries {
			if !entry.Agent && !entry.Human {
				continue
			}
			if !sidebarTagText(taskID) || !sidebarTagText(entry.TagID) {
				return nil, errSidebarGroupingState
			}
			if _, exists := known[entry.TagID]; !exists {
				continue
			}
			pair := pluginsdk.SidebarGroupingMembership{TaskID: taskID, GroupID: entry.TagID}
			if _, duplicate := pairs[pair]; duplicate {
				continue
			}
			if len(pairs) == pluginsdk.MaxSidebarGroupingMemberships {
				return nil, errSidebarGroupingState
			}
			pairs[pair] = struct{}{}
		}
	}
	members := make([]pluginsdk.SidebarGroupingMembership, len(pairs))
	i := 0
	for pair := range pairs {
		members[i] = pair
		i++
	}
	return completeSidebarSnapshot(groups, members), nil
}

// completeSidebarSnapshot orders values and memberships deterministically and
// derives the content revision from that canonical order.
func completeSidebarSnapshot(groups []pluginsdk.SidebarGroupingValue, members []pluginsdk.SidebarGroupingMembership) *pluginsdk.SidebarGroupingSnapshot {
	slices.SortFunc(groups, func(a, b pluginsdk.SidebarGroupingValue) int { return cmp.Compare(a.ID, b.ID) })
	slices.SortFunc(members, func(a, b pluginsdk.SidebarGroupingMembership) int {
		if order := cmp.Compare(a.GroupID, b.GroupID); order != 0 {
			return order
		}
		return cmp.Compare(a.TaskID, b.TaskID)
	})
	return &pluginsdk.SidebarGroupingSnapshot{Version: 1, Revision: sidebarTagRevision(groups, members), Complete: true, Groups: groups, Memberships: members}
}

func sidebarTagText(value string) bool {
	return strings.TrimSpace(value) != "" && len(value) <= 256 && utf8.ValidString(value) && strings.IndexFunc(value, unicode.IsControl) < 0
}

// Length-prefixed fields preserve boundaries without a second JSON encoding.
// Only visible classification participates, not ownership, notes or timestamps.
func sidebarTagRevision(groups []pluginsdk.SidebarGroupingValue, members []pluginsdk.SidebarGroupingMembership) string {
	digest := sha256.New()
	var length [8]byte
	var field [256]byte
	writeCount := func(count int) { binary.BigEndian.PutUint64(length[:], uint64(count)); digest.Write(length[:]) }
	writeField := func(value string) { writeCount(len(value)); n := copy(field[:], value); digest.Write(field[:n]) }
	writeCount(len(groups))
	for _, group := range groups {
		writeField(group.ID)
		writeField(group.Label)
		writeField(group.Color)
	}
	writeCount(len(members))
	for _, pair := range members {
		writeField(pair.TaskID)
		writeField(pair.GroupID)
	}
	var sum [sha256.Size]byte
	var encoded [sha256.Size * 2]byte
	hex.Encode(encoded[:], digest.Sum(sum[:0]))
	return string(encoded[:])
}
