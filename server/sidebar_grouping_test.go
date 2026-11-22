package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func sidebarTestDoc() tagDoc {
	doc := newTagDoc()
	doc.Tags = []sharedTag{
		{ID: "tag-bug", Name: "Bug", Color: "#ef4444"},
		{ID: "tag-ux", Name: "UX", Color: "#2b6"},
		{ID: "tag-literal", Name: "Bug + UX", Color: "#000000"},
		{ID: "tag-unused", Name: "Unused", Color: "#111111"},
	}
	doc.Tasks = map[string][]taskTag{
		"task-multi":    {{TagID: "tag-ux", Human: true}, {TagID: "tag-bug", Human: true}},
		"task-reversed": {{TagID: "tag-bug", Human: true}, {TagID: "tag-ux", Agent: true}},
		"task-bug":      {{TagID: "tag-bug", Human: true}},
		"task-literal":  {{TagID: "tag-literal", Human: true}},
		"task-orphan":   {{TagID: "tag-deleted", Human: true}},
	}
	return doc
}

func sidebarAction(t *testing.T, doc tagDoc, key string) sidebarGroupingSnapshot {
	t.Helper()
	p, host := newAgentTagTestPlugin()
	raw, err := encodeTagDoc(doc)
	require.NoError(t, err)
	require.NoError(t, host.SetState(context.Background(), "workspace", "ws-1", tagStateKey, raw))
	response, err := p.HandleAction(context.Background(), actionReq(key, []byte(`{}`)))
	require.NoError(t, err)
	// Exact canonical field names and both arrays present, even when empty:
	// the host rejects aliases and missing arrays as unavailable.
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(response.Body, &fields))
	for _, name := range []string{"version", "revision", "complete", "groups", "memberships"} {
		require.Contains(t, fields, name)
	}
	require.NotEqual(t, "null", string(fields["groups"]))
	require.NotEqual(t, "null", string(fields["memberships"]))
	var snapshot sidebarGroupingSnapshot
	require.NoError(t, json.Unmarshal(response.Body, &snapshot))
	require.Equal(t, 1, snapshot.Version)
	require.True(t, snapshot.Complete)
	return snapshot
}

func groupsOf(snapshot sidebarGroupingSnapshot, taskID string) []string {
	labels := map[string]string{}
	for _, group := range snapshot.Groups {
		labels[group.ID] = group.Label
	}
	var out []string
	for _, member := range snapshot.Memberships {
		if member.TaskID == taskID {
			out = append(out, labels[member.GroupID])
		}
	}
	return out
}

func TestSidebarTagGroupingRepeatsMultiTagTasks(t *testing.T) {
	snapshot := sidebarAction(t, sidebarTestDoc(), sidebarTagsAction)
	require.ElementsMatch(t, []string{"Bug", "UX"}, groupsOf(snapshot, "task-multi"))
	require.Equal(t, []string{"Bug"}, groupsOf(snapshot, "task-bug"))
	require.Empty(t, groupsOf(snapshot, "task-orphan"), "a deleted tag leaves the task for the host's Untagged group")
	labels := make([]string, len(snapshot.Groups))
	for i, group := range snapshot.Groups {
		labels[i] = group.Label
		if group.Label == "UX" {
			require.Equal(t, "#22bb66", group.Color, "the #rgb shorthand becomes the host's #RRGGBB form")
		}
	}
	require.ElementsMatch(t, []string{"Bug", "UX", "Bug + UX"}, labels, "unused catalog tags create no group")
}

func TestSidebarTagCombinationGroupsExactSets(t *testing.T) {
	snapshot := sidebarAction(t, sidebarTestDoc(), sidebarTagCombinationsAction)
	require.Equal(t, []string{"Bug + UX"}, groupsOf(snapshot, "task-multi"))
	require.Equal(t, []string{"Bug"}, groupsOf(snapshot, "task-bug"))
	group := map[string]string{}
	for _, member := range snapshot.Memberships {
		group[member.TaskID] = member.GroupID
	}
	require.Equal(t, group["task-multi"], group["task-reversed"], "assignment order does not split a combination")
	require.NotEqual(t, group["task-multi"], group["task-literal"], "a tag named like a combination stays its own group")
	for _, value := range snapshot.Groups {
		switch value.ID {
		case group["task-multi"]:
			require.Empty(t, value.Color, "a combination borrows no member color")
		case group["task-bug"]:
			require.Equal(t, "#ef4444", value.Color)
		}
	}
}

func TestSidebarGroupingRevisionTracksClassification(t *testing.T) {
	doc := sidebarTestDoc()
	first := sidebarAction(t, doc, sidebarTagCombinationsAction)
	require.Equal(t, first, sidebarAction(t, doc, sidebarTagCombinationsAction), "an unchanged classification keeps its revision")

	doc.Tags[0].Name = "Defect"
	renamed := sidebarAction(t, doc, sidebarTagCombinationsAction)
	require.NotEqual(t, first.Revision, renamed.Revision)
	require.Equal(t, []string{"Defect + UX"}, groupsOf(renamed, "task-multi"))
	require.Equal(t, first.Memberships, renamed.Memberships, "a rename keeps every group identity")

	doc.Tasks["task-bug"] = append(doc.Tasks["task-bug"], taskTag{TagID: "tag-ux", Human: true})
	moved := sidebarAction(t, doc, sidebarTagCombinationsAction)
	require.NotEqual(t, renamed.Revision, moved.Revision)
	require.Equal(t, []string{"Defect + UX"}, groupsOf(moved, "task-bug"))
}

func TestSidebarGroupingEmptyWorkspace(t *testing.T) {
	snapshot := sidebarAction(t, newTagDoc(), sidebarTagsAction)
	require.Empty(t, snapshot.Groups)
	require.Empty(t, snapshot.Memberships)
}

func TestSidebarGroupingFailsInsteadOfTruncating(t *testing.T) {
	doc := newTagDoc()
	for i := range sidebarGroupingMaxValues + 1 {
		id := fmt.Sprintf("tag-%04d", i)
		doc.Tags = append(doc.Tags, sharedTag{ID: id, Name: id})
		doc.Tasks[fmt.Sprintf("task-%04d", i)] = []taskTag{{TagID: id, Human: true}}
	}
	_, err := sidebarTagSnapshot(doc)
	require.ErrorContains(t, err, "groups")

	doc = newTagDoc()
	for i := range 50 {
		doc.Tags = append(doc.Tags, sharedTag{ID: fmt.Sprintf("tag-%02d", i), Name: fmt.Sprintf("t%02d", i)})
	}
	for task := range 201 {
		var entries []taskTag
		for _, tag := range doc.Tags {
			entries = append(entries, taskTag{TagID: tag.ID, Human: true})
		}
		doc.Tasks[fmt.Sprintf("task-%03d", task)] = entries
	}
	_, err = sidebarTagSnapshot(doc)
	require.ErrorContains(t, err, "memberships")
}

func TestSidebarCombinationLabelFitsHostLimit(t *testing.T) {
	doc := newTagDoc()
	var entries []taskTag
	for i := range 20 {
		id := fmt.Sprintf("tag-%02d", i)
		doc.Tags = append(doc.Tags, sharedTag{ID: id, Name: strings.Repeat("é", 11) + fmt.Sprintf("%02d", i)})
		entries = append(entries, taskTag{TagID: id, Human: true})
	}
	doc.Tasks["task-many"] = entries
	snapshot, err := sidebarTagCombinationSnapshot(doc)
	require.NoError(t, err)
	label := snapshot.Groups[0].Label
	require.LessOrEqual(t, len(label), sidebarGroupingMaxText)
	require.True(t, utf8.ValidString(label))
	require.True(t, strings.HasSuffix(label, "…"))
}
