package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// Kandev's complete saved-sidebar grouping contract (manifest
// `sidebar_groupings`). The host invokes these workspace actions with an empty
// body and joins the returned memberships server-side, before its 100-row page
// boundary, so classification covers the whole filtered view rather than the
// rows a browser happened to load.
const (
	sidebarTagsAction            = "sidebar-tags"
	sidebarTagCombinationsAction = "sidebar-tag-combinations"

	// Host limits: exceeding any of them must fail the action rather than
	// return a truncated snapshot, which the host would treat as complete.
	sidebarGroupingMaxValues      = 1000
	sidebarGroupingMaxMemberships = 10000
	sidebarGroupingMaxBytes       = 1 << 20
	sidebarGroupingMaxText        = 256
	sidebarCombinationSeparator   = " + "
)

var (
	sidebarGroupingColor      = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
	sidebarGroupingShortColor = regexp.MustCompile(`^#[0-9a-fA-F]{3}$`)
)

type sidebarGroupingValue struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Color string `json:"color,omitempty"`
}

type sidebarGroupingMembership struct {
	TaskID  string `json:"task_id"`
	GroupID string `json:"group_id"`
}

type sidebarGroupingSnapshot struct {
	Version     int                         `json:"version"`
	Revision    string                      `json:"revision"`
	Complete    bool                        `json:"complete"`
	Groups      []sidebarGroupingValue      `json:"groups"`
	Memberships []sidebarGroupingMembership `json:"memberships"`
}

// sidebarGroupingAction answers the host's classification request. The body
// is ignored: the host always sends an empty object.
func (p *tagsPlugin) sidebarGroupingAction(ctx context.Context, req *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	if err := requireActionWorkspace(req); err != nil {
		return nil, err
	}
	doc, err := p.readTagDoc(ctx, req.Context.WorkspaceID)
	if err != nil {
		return nil, err
	}
	build := sidebarTagSnapshot
	if req.ActionKey == sidebarTagCombinationsAction {
		build = sidebarTagCombinationSnapshot
	}
	snapshot, err := build(doc)
	if err != nil {
		return nil, err
	}
	body, err := encodeSidebarSnapshot(snapshot)
	if err != nil {
		return nil, err
	}
	return &pluginsdk.PluginActionResponse{Body: body, Headers: map[string]string{"Content-Type": "application/json"}}, nil
}

// sidebarTaskTags resolves each tagged task to its distinct catalog tags,
// ordered by name. Orphaned applications and IDs the host would reject are
// dropped so one bad record cannot make the whole snapshot unavailable.
func sidebarTaskTags(doc tagDoc) map[string][]sharedTag {
	out := make(map[string][]sharedTag, len(doc.Tasks))
	for taskID, entries := range doc.Tasks {
		if !sidebarGroupingText(taskID) {
			continue
		}
		seen := map[string]bool{}
		var tags []sharedTag
		for _, entry := range entries {
			tag := findTag(doc.Tags, entry.TagID)
			if tag == nil || seen[tag.ID] || !sidebarGroupingText(tag.ID) || !sidebarGroupingText(tag.Name) {
				continue
			}
			seen[tag.ID] = true
			tags = append(tags, *tag)
		}
		if len(tags) == 0 {
			continue
		}
		sort.Slice(tags, func(i, j int) bool {
			a, b := strings.ToLower(tags[i].Name), strings.ToLower(tags[j].Name)
			if a != b {
				return a < b
			}
			return tags[i].ID < tags[j].ID
		})
		out[taskID] = tags
	}
	return out
}

// sidebarTagSnapshot puts every task in each of its tags' groups, matching the
// board's per-tag view: a two-tag task appears under both tags.
func sidebarTagSnapshot(doc tagDoc) (sidebarGroupingSnapshot, error) {
	groups := map[string]sidebarGroupingValue{}
	var memberships []sidebarGroupingMembership
	for taskID, tags := range sidebarTaskTags(doc) {
		for _, tag := range tags {
			groups[tag.ID] = sidebarGroupingValue{ID: tag.ID, Label: tag.Name, Color: sidebarColor(tag.Color)}
			memberships = append(memberships, sidebarGroupingMembership{TaskID: taskID, GroupID: tag.ID})
		}
	}
	return newSidebarSnapshot(groups, memberships)
}

// sidebarTagCombinationSnapshot puts every task in exactly one group: its
// exact tag set. The group ID derives from sorted tag IDs, so assignment order
// does not matter, renames keep the identity, and a tag literally named
// "Bug + UX" cannot collide with the two-tag set.
func sidebarTagCombinationSnapshot(doc tagDoc) (sidebarGroupingSnapshot, error) {
	groups := map[string]sidebarGroupingValue{}
	var memberships []sidebarGroupingMembership
	for taskID, tags := range sidebarTaskTags(doc) {
		value := sidebarCombinationValue(tags)
		groups[value.ID] = value
		memberships = append(memberships, sidebarGroupingMembership{TaskID: taskID, GroupID: value.ID})
	}
	return newSidebarSnapshot(groups, memberships)
}

func sidebarCombinationValue(tags []sharedTag) sidebarGroupingValue {
	ids := make([]string, len(tags))
	names := make([]string, len(tags))
	for i, tag := range tags {
		ids[i] = tag.ID
		names[i] = tag.Name
	}
	sort.Strings(ids)
	sum := sha256.Sum256([]byte(strings.Join(ids, "\x00")))
	value := sidebarGroupingValue{
		ID:    "set-" + hex.EncodeToString(sum[:]),
		Label: truncateSidebarText(strings.Join(names, sidebarCombinationSeparator)),
	}
	// A combination has no single color; borrowing one member's would imply
	// that tag dominates. Single-tag sets keep their tag's color.
	if len(tags) == 1 {
		value.Color = sidebarColor(tags[0].Color)
	}
	return value
}

func newSidebarSnapshot(groups map[string]sidebarGroupingValue, memberships []sidebarGroupingMembership) (sidebarGroupingSnapshot, error) {
	if len(groups) > sidebarGroupingMaxValues {
		return sidebarGroupingSnapshot{}, fmt.Errorf("sidebar grouping has %d groups, above the host limit of %d", len(groups), sidebarGroupingMaxValues)
	}
	if len(memberships) > sidebarGroupingMaxMemberships {
		return sidebarGroupingSnapshot{}, fmt.Errorf("sidebar grouping has %d memberships, above the host limit of %d", len(memberships), sidebarGroupingMaxMemberships)
	}
	values := make([]sidebarGroupingValue, 0, len(groups))
	for _, value := range groups {
		values = append(values, value)
	}
	sort.Slice(values, func(i, j int) bool { return values[i].ID < values[j].ID })
	if memberships == nil {
		memberships = []sidebarGroupingMembership{}
	}
	sort.Slice(memberships, func(i, j int) bool {
		if memberships[i].TaskID != memberships[j].TaskID {
			return memberships[i].TaskID < memberships[j].TaskID
		}
		return memberships[i].GroupID < memberships[j].GroupID
	})
	// The revision is a digest of the sorted classification, so it stays stable
	// while nothing changes and moves on any label, color, or membership change.
	canonical, err := json.Marshal(struct {
		Groups      []sidebarGroupingValue      `json:"groups"`
		Memberships []sidebarGroupingMembership `json:"memberships"`
	}{values, memberships})
	if err != nil {
		return sidebarGroupingSnapshot{}, err
	}
	sum := sha256.Sum256(canonical)
	return sidebarGroupingSnapshot{
		Version:     1,
		Revision:    "tags-" + hex.EncodeToString(sum[:]),
		Complete:    true,
		Groups:      values,
		Memberships: memberships,
	}, nil
}

func encodeSidebarSnapshot(snapshot sidebarGroupingSnapshot) ([]byte, error) {
	body, err := json.Marshal(snapshot)
	if err != nil {
		return nil, err
	}
	if len(body) > sidebarGroupingMaxBytes {
		return nil, fmt.Errorf("sidebar grouping response is %d bytes, above the host limit of %d", len(body), sidebarGroupingMaxBytes)
	}
	return body, nil
}

// sidebarColor returns the host's #RRGGBB form. Tags may store the #rgb
// shorthand the agent tools accept, which the host would otherwise reject.
func sidebarColor(color string) string {
	if sidebarGroupingShortColor.MatchString(color) {
		color = "#" + strings.Repeat(color[1:2], 2) + strings.Repeat(color[2:3], 2) + strings.Repeat(color[3:4], 2)
	}
	if sidebarGroupingColor.MatchString(color) {
		return color
	}
	return ""
}

func sidebarGroupingText(value string) bool {
	return strings.TrimSpace(value) != "" && len(value) <= sidebarGroupingMaxText &&
		utf8.ValidString(value) && strings.IndexFunc(value, unicode.IsControl) < 0
}

// truncateSidebarText keeps long combination labels within the host's byte
// limit on a rune boundary. The group ID, not the label, is the identity.
func truncateSidebarText(value string) string {
	if len(value) <= sidebarGroupingMaxText {
		return value
	}
	const ellipsis = "…"
	cut := sidebarGroupingMaxText - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + ellipsis
}
