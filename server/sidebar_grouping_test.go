package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/require"
)

// Both declared groupings share the strict shared-state reader, so corrupt or
// over-bound input must fail the same way through either action.
var sidebarGroupingActions = []string{"sidebar-tags", "sidebar-multi-tags"}

func readSidebarSnapshot(t *testing.T, p *tagsPlugin, workspace string) *pluginsdk.SidebarGroupingSnapshot {
	t.Helper()
	return readSidebarActionSnapshot(t, p, workspace, "sidebar-tags")
}

func readSidebarActionSnapshot(t *testing.T, p *tagsPlugin, workspace, action string) *pluginsdk.SidebarGroupingSnapshot {
	t.Helper()
	req := actionReq(action, []byte(`{"workspace_id":"foreign"}`))
	req.Context.WorkspaceID = workspace
	response, err := p.HandleAction(t.Context(), req)
	require.NoError(t, err)
	snapshot, err := pluginsdk.DecodeSidebarGroupingSnapshot(response.Body)
	require.NoError(t, err)
	return snapshot
}

func storeSidebarDoc(t *testing.T, host *fakeHost, workspace string, doc tagDoc) {
	t.Helper()
	raw, err := encodeTagDoc(doc)
	require.NoError(t, err)
	require.NoError(t, host.SetState(t.Context(), "workspace", workspace, tagStateKey, raw))
}

func TestSidebarGroupingExportsCompleteSharedPresenceWithoutMutation(t *testing.T) {
	p, host := newAgentTagTestPlugin()
	doc := newTagDoc()
	doc.Tags = []sharedTag{{ID: "b", Name: "Alpha", Color: "#AbC"}, {ID: "a", Name: "Beta", Color: "#ff8800"}}
	for i := range 101 {
		doc.Tasks[fmt.Sprintf("task-%03d", i)] = []taskTag{{TagID: "b", Agent: true, Human: true}, {TagID: "b", Human: true}}
	}
	doc.Tasks["task-100"] = append(doc.Tasks["task-100"], taskTag{TagID: "a", Human: true}, taskTag{TagID: "deleted", Human: true})
	doc.Tasks["inactive"] = []taskTag{{TagID: "a"}}
	storeSidebarDoc(t, host, "ws-1", doc)
	foreign := newTagDoc()
	foreign.Tags = []sharedTag{{ID: "secret", Name: "Other workspace", Color: "#112233"}}
	storeSidebarDoc(t, host, "foreign", foreign)
	_, setsBefore, deletesBefore := host.stateCallCounts()
	snapshot := readSidebarSnapshot(t, p, "ws-1")
	require.ElementsMatch(t, []pluginsdk.SidebarGroupingValue{{ID: "a", Label: "Beta", Color: "#ff8800"}, {ID: "b", Label: "Alpha", Color: "#aabbcc"}}, snapshot.Groups)
	require.Len(t, snapshot.Memberships, 102)
	require.Contains(t, snapshot.Memberships, pluginsdk.SidebarGroupingMembership{TaskID: "task-100", GroupID: "a"})
	require.Contains(t, snapshot.Memberships, pluginsdk.SidebarGroupingMembership{TaskID: "task-100", GroupID: "b"})
	for _, pair := range snapshot.Memberships {
		require.NotEqual(t, "inactive", pair.TaskID)
		require.NotEqual(t, "deleted", pair.GroupID)
	}
	_, setsAfter, deletesAfter := host.stateCallCounts()
	require.Equal(t, setsBefore, setsAfter)
	require.Equal(t, deletesBefore, deletesAfter)
	doc.Tags[0], doc.Tags[1] = doc.Tags[1], doc.Tags[0]
	doc.Tasks["task-000"] = []taskTag{{TagID: "b", Human: true, Note: "different annotation", UpdatedAt: "2099-01-01T00:00:00Z"}}
	storeSidebarDoc(t, host, "ws-1", doc)
	replacement := &tagsPlugin{}
	replacement.SetHost(host)
	reordered := readSidebarSnapshot(t, replacement, "ws-1")
	require.Equal(t, snapshot.Revision, reordered.Revision)
	require.ElementsMatch(t, snapshot.Memberships, reordered.Memberships)
	require.Equal(t, []pluginsdk.SidebarGroupingValue{{ID: "secret", Label: "Other workspace", Color: "#112233"}}, readSidebarSnapshot(t, p, "foreign").Groups)
}

func TestSidebarGroupingRevisionTracksClassificationMutations(t *testing.T) {
	p, _ := newAgentTagTestPlugin()
	empty := readSidebarSnapshot(t, p, "ws-1")
	created, err := p.HandleAction(t.Context(), actionReq("tag-create", []byte(`{"name":"Ready","color":"#abc"}`)))
	require.NoError(t, err)
	id := decodeActionBody(t, created)["tags"].([]any)[0].(map[string]any)["id"].(string)
	catalog := readSidebarSnapshot(t, p, "ws-1")
	require.NotEqual(t, empty.Revision, catalog.Revision)
	require.Equal(t, []pluginsdk.SidebarGroupingValue{{ID: id, Label: "Ready", Color: "#aabbcc"}}, catalog.Groups)
	_, err = p.HandleAction(t.Context(), actionReq("task-tag-add", []byte(`{"tagId":"`+id+`"}`)))
	require.NoError(t, err)
	applied := readSidebarSnapshot(t, p, "ws-1")
	require.NotEqual(t, catalog.Revision, applied.Revision)
	require.Equal(t, []pluginsdk.SidebarGroupingMembership{{TaskID: "task-1", GroupID: id}}, applied.Memberships)
	_, err = p.HandleAction(t.Context(), actionReq("tag-update", []byte(`{"id":"`+id+`","name":"Done"}`)))
	require.NoError(t, err)
	renamed := readSidebarSnapshot(t, p, "ws-1")
	require.NotEqual(t, applied.Revision, renamed.Revision)
	require.Equal(t, "Done", renamed.Groups[0].Label)
	_, err = p.HandleAction(t.Context(), actionReq("tag-update", []byte(`{"id":"`+id+`","color":"#123"}`)))
	require.NoError(t, err)
	recolored := readSidebarSnapshot(t, p, "ws-1")
	require.NotEqual(t, renamed.Revision, recolored.Revision)
	require.Equal(t, "#112233", recolored.Groups[0].Color)
	_, err = p.HandleAction(t.Context(), actionReq("task-tag-remove", []byte(`{"tagId":"`+id+`"}`)))
	require.NoError(t, err)
	removed := readSidebarSnapshot(t, p, "ws-1")
	require.NotEqual(t, recolored.Revision, removed.Revision)
	require.Empty(t, removed.Memberships)
	_, err = p.HandleAction(t.Context(), actionReq("tag-delete", []byte(`{"id":"`+id+`"}`)))
	require.NoError(t, err)
	require.Equal(t, empty.Revision, readSidebarSnapshot(t, p, "ws-1").Revision)
}

func TestSidebarGroupingDeduplicatesHumanAndAgentApplications(t *testing.T) {
	p, _ := newAgentTagTestPlugin()
	id := agentCreate(t, p, "Agent classification")
	result, err := p.InvokeAgentTool(t.Context(), agentToolReq("add_tag", map[string]any{"tag_id": id, "note": "first annotation"}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	agent := readSidebarSnapshot(t, p, "ws-1")
	_, err = p.HandleAction(t.Context(), actionReq("task-tag-add", []byte(`{"tagId":"`+id+`"}`)))
	require.NoError(t, err)
	require.Equal(t, agent.Revision, readSidebarSnapshot(t, p, "ws-1").Revision)
	result, err = p.InvokeAgentTool(t.Context(), agentToolReq("add_tag", map[string]any{"tag_id": id, "note": "different annotation"}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	require.Equal(t, agent.Revision, readSidebarSnapshot(t, p, "ws-1").Revision)
	result, err = p.InvokeAgentTool(t.Context(), agentToolReq("remove_tag", map[string]any{"tag_id": id}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	human := readSidebarSnapshot(t, p, "ws-1")
	require.Equal(t, agent.Revision, human.Revision)
	require.Equal(t, []pluginsdk.SidebarGroupingMembership{{TaskID: "task-1", GroupID: id}}, human.Memberships)
}

func TestSidebarGroupingRejectsInvalidOrIncompleteStoredState(t *testing.T) {
	cases := map[string]map[string]any{
		"unsupported version":     {"version": 3, "tags": []any{}, "tasks": map[string]any{}},
		"missing catalog":         {"version": 2, "tasks": map[string]any{}},
		"missing applications":    {"version": 2, "tags": []any{}},
		"malformed catalog":       {"version": 2, "tags": "bad", "tasks": map[string]any{}},
		"empty existing document": {},
	}
	for name, raw := range cases {
		for _, action := range sidebarGroupingActions {
			t.Run(action+"/"+name, func(t *testing.T) {
				p, host := newAgentTagTestPlugin()
				require.NoError(t, host.SetState(t.Context(), "workspace", "ws-1", tagStateKey, raw))
				response, err := p.HandleAction(t.Context(), actionReq(action, nil))
				require.Error(t, err)
				require.Nil(t, response)
			})
		}
	}
	for _, testCase := range []struct {
		name   string
		tag    sharedTag
		pair   taskTag
		taskID string
	}{
		{"blank id", sharedTag{Name: "Name", Color: "#123456"}, taskTag{}, "task"},
		{"control label", sharedTag{ID: "g", Name: "Bad\nName", Color: "#123456"}, taskTag{}, "task"},
		{"invalid color", sharedTag{ID: "g", Name: "Name", Color: "url(secret)"}, taskTag{}, "task"},
		{"invalid task", sharedTag{ID: "g", Name: "Name", Color: "#123456"}, taskTag{TagID: "g", Human: true}, ""},
		{"invalid application", sharedTag{ID: "g", Name: "Name", Color: "#123456"}, taskTag{Human: true}, "task"},
	} {
		for _, action := range sidebarGroupingActions {
			t.Run(action+"/"+testCase.name, func(t *testing.T) {
				p, host := newAgentTagTestPlugin()
				doc := newTagDoc()
				doc.Tags = []sharedTag{testCase.tag}
				doc.Tasks[testCase.taskID] = []taskTag{testCase.pair}
				storeSidebarDoc(t, host, "ws-1", doc)
				response, err := p.HandleAction(t.Context(), actionReq(action, nil))
				require.Error(t, err)
				require.Nil(t, response)
			})
		}
	}
}

func TestSidebarGroupingRejectsCorruptApplicationShapesWithoutWrites(t *testing.T) {
	cases := map[string]any{
		"null list":      nil,
		"null row":       []any{nil},
		"missing tag ID": []any{map[string]any{}},
		"null tag ID":    []any{map[string]any{"tag_id": nil}},
		"numeric tag ID": []any{map[string]any{"tag_id": 1}},
		"null human":     []any{map[string]any{"tag_id": "g", "human": nil}},
		"null agent":     []any{map[string]any{"tag_id": "g", "agent": nil}},
		"untyped human":  []any{map[string]any{"tag_id": "g", "human": "true"}},
		"untyped agent":  []any{map[string]any{"tag_id": "g", "agent": 1}},
	}
	for name, entries := range cases {
		for _, action := range sidebarGroupingActions {
			t.Run(action+"/"+name, func(t *testing.T) {
				p, host := newAgentTagTestPlugin()
				raw := map[string]any{
					"version": 2,
					"tags":    []any{map[string]any{"id": "g", "name": "Ready", "color": "#123456"}},
					"tasks":   map[string]any{"task-1": entries},
				}
				require.NoError(t, host.SetState(t.Context(), "workspace", "ws-1", tagStateKey, raw))
				_, setsBefore, deletesBefore := host.stateCallCounts()
				response, err := p.HandleAction(t.Context(), actionReq(action, nil))
				require.Error(t, err)
				require.Nil(t, response)
				_, setsAfter, deletesAfter := host.stateCallCounts()
				require.Equal(t, setsBefore, setsAfter)
				require.Equal(t, deletesBefore, deletesAfter)
			})
		}
	}
}

func TestSidebarGroupingAcceptsEmptyAndInactiveTypedApplications(t *testing.T) {
	for _, entries := range [][]taskTag{{}, {{TagID: "g"}}, {{TagID: "g", Agent: false, Human: false}}} {
		p, host := newAgentTagTestPlugin()
		doc := newTagDoc()
		doc.Tags = []sharedTag{{ID: "g", Name: "Ready", Color: "#123456"}}
		doc.Tasks["task-1"] = entries
		storeSidebarDoc(t, host, "ws-1", doc)
		snapshot := readSidebarSnapshot(t, p, "ws-1")
		require.Empty(t, snapshot.Memberships)
		require.Equal(t, []pluginsdk.SidebarGroupingValue{{ID: "g", Label: "Ready", Color: "#123456"}}, snapshot.Groups)
	}
}

func TestSidebarGroupingBoundsRawApplicationsBeforeLegacyOrDeduplication(t *testing.T) {
	for _, version := range []int{1, 2} {
		for _, count := range []int{pluginsdk.MaxSidebarGroupingMemberships, pluginsdk.MaxSidebarGroupingMemberships + 1} {
			for _, action := range sidebarGroupingActions {
				t.Run(fmt.Sprintf("%s/v%d/%d", action, version, count), func(t *testing.T) {
					p, host := newAgentTagTestPlugin()
					entry := map[string]any{"tag": "ready"}
					groupID := "agent-legacy-ready"
					if version == 2 {
						entry = map[string]any{"tag_id": "g", "human": true}
						groupID = "g"
					}
					if action == "sidebar-multi-tags" {
						groupID = expectedSetID(`["` + groupID + `"]`)
					}
					entries := make([]any, count)
					for i := range entries {
						entries[i] = entry
					}
					raw := map[string]any{"version": version, "tasks": map[string]any{"task-1": entries}}
					if version == 2 {
						raw["tags"] = []any{map[string]any{"id": "g", "name": "Ready", "color": "#123456"}}
					}
					require.NoError(t, host.SetState(t.Context(), "workspace", "ws-1", tagStateKey, raw))
					_, setsBefore, deletesBefore := host.stateCallCounts()
					response, err := p.HandleAction(t.Context(), actionReq(action, nil))
					if count > pluginsdk.MaxSidebarGroupingMemberships {
						require.Error(t, err)
						require.Nil(t, response)
					} else {
						require.NoError(t, err)
						snapshot, err := pluginsdk.DecodeSidebarGroupingSnapshot(response.Body)
						require.NoError(t, err)
						require.Equal(t, []pluginsdk.SidebarGroupingMembership{{TaskID: "task-1", GroupID: groupID}}, snapshot.Memberships)
					}
					_, setsAfter, deletesAfter := host.stateCallCounts()
					require.Equal(t, setsBefore, setsAfter)
					require.Equal(t, deletesBefore, deletesAfter)
				})
			}
		}
	}
}

func TestSidebarGroupingEnforcesBoundsWithoutTruncation(t *testing.T) {
	for _, kind := range []string{"groups", "memberships", "response bytes", "duplicate definitions"} {
		t.Run(kind, func(t *testing.T) {
			p, host := newAgentTagTestPlugin()
			doc := newTagDoc()
			doc.Tags = []sharedTag{{ID: "g", Name: "Value", Color: "#123456"}}
			switch kind {
			case "groups":
				for i := range pluginsdk.MaxSidebarGroupingValues {
					doc.Tags = append(doc.Tags, sharedTag{ID: fmt.Sprintf("g-%d", i), Name: "Value", Color: "#123456"})
				}
			case "memberships":
				for i := range pluginsdk.MaxSidebarGroupingMemberships + 1 {
					doc.Tasks[fmt.Sprintf("task-%05d", i)] = []taskTag{{TagID: "g", Human: true}}
				}
			case "response bytes":
				doc.Tags[0].ID = strings.Repeat("g", 256)
				for i := range pluginsdk.MaxSidebarGroupingMemberships {
					doc.Tasks[fmt.Sprintf("%0256d", i)] = []taskTag{{TagID: doc.Tags[0].ID, Human: true}}
				}
			case "duplicate definitions":
				doc.Tags = append(doc.Tags, sharedTag{ID: "g", Name: "Other", Color: "#123456"})
			}
			storeSidebarDoc(t, host, "ws-1", doc)
			response, err := p.HandleAction(t.Context(), actionReq("sidebar-tags", nil))
			require.Error(t, err)
			require.Nil(t, response)
		})
	}
}

type unavailableSidebarHost struct {
	*fakeHost
	err error
}

func (h unavailableSidebarHost) GetState(context.Context, string, string, string) (map[string]any, bool, error) {
	return nil, false, h.err
}

func TestSidebarGroupingDoesNotTurnReadFailureOrCancellationIntoEmptySuccess(t *testing.T) {
	p, host := newAgentTagTestPlugin()
	failure := errors.New("state read failed")
	p.SetHost(unavailableSidebarHost{fakeHost: host, err: failure})
	response, err := p.HandleAction(t.Context(), actionReq("sidebar-tags", nil))
	require.ErrorIs(t, err, failure)
	require.Nil(t, response)
	p.SetHost(host)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	response, err = p.HandleAction(ctx, actionReq("sidebar-tags", nil))
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, response)
	require.Zero(t, host.getCalls)
}
