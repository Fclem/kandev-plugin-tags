package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSidebarGroupingDistinguishesControlNamesFromLiteralEscapes(t *testing.T) {
	p, host := newAgentTagTestPlugin()
	control, literal := "Ready\tNow", `Ready\tNow`
	created, err := p.HandleAction(t.Context(), actionReq("tag-create", []byte(`{"name":"Ready\tNow","color":"#abc"}`)))
	require.NoError(t, err)
	id := decodeActionBody(t, created)["tags"].([]any)[0].(map[string]any)["id"].(string)
	_, err = p.HandleAction(t.Context(), actionReq("task-tag-add", []byte(`{"tagId":"`+id+`"}`)))
	require.NoError(t, err)
	beforeTag := readSidebarSnapshot(t, p, "ws-1")
	beforeMulti := readMultiTagSnapshot(t, p)
	payload, err := json.Marshal(map[string]string{"id": id, "name": literal})
	require.NoError(t, err)
	_, err = p.HandleAction(t.Context(), actionReq("tag-update", payload))
	require.NoError(t, err)
	afterTag := readSidebarSnapshot(t, p, "ws-1")
	afterMulti := readMultiTagSnapshot(t, p)
	require.Equal(t, beforeTag.Groups[0].ID, afterTag.Groups[0].ID)
	require.Equal(t, beforeMulti.Groups[0].ID, afterMulti.Groups[0].ID)
	require.NotEqual(t, beforeTag.Revision, afterTag.Revision)
	require.NotEqual(t, beforeMulti.Revision, afterMulti.Revision)
	created, err = p.HandleAction(t.Context(), actionReq("tag-create", []byte(`{"name":"Ready\tNow","color":"#abc"}`)))
	require.NoError(t, err)
	var controlID string
	for _, raw := range decodeActionBody(t, created)["tags"].([]any) {
		tag := raw.(map[string]any)
		if tag["name"] == control {
			controlID = tag["id"].(string)
		}
	}
	require.NotEmpty(t, controlID)
	req := actionReq("task-tag-add", []byte(`{"tagId":"`+controlID+`"}`))
	req.Context.TaskID = "task-2"
	_, err = p.HandleAction(t.Context(), req)
	require.NoError(t, err)
	_, sets, deletes := host.stateCallCounts()
	require.Equal(t, map[string][]string{`Ready\\tNow`: {"task-1"}, `Ready\tNow`: {"task-2"}}, tasksByLabel(t, readSidebarSnapshot(t, p, "ws-1")))
	quotedControl, err := json.Marshal(control)
	require.NoError(t, err)
	quotedLiteral, err := json.Marshal(literal)
	require.NoError(t, err)
	require.Equal(t, map[string][]string{string(quotedControl): {"task-2"}, string(quotedLiteral): {"task-1"}}, tasksByLabel(t, readMultiTagSnapshot(t, p)))
	_, afterSets, afterDeletes := host.stateCallCounts()
	require.Equal(t, sets, afterSets)
	require.Equal(t, deletes, afterDeletes)
}

func TestSidebarMultiTagQuotedControlNamesDecodeToTheOriginalName(t *testing.T) {
	for _, name := range []string{"Ready\aNow", "Ready\vNow", "Ready\u0085Now", `Ready\"Now`} {
		t.Run(name, func(t *testing.T) {
			p, host := newAgentTagTestPlugin()
			doc := newTagDoc()
			doc.Tags = []sharedTag{{ID: "g", Name: name, Color: "#abc"}}
			doc.Tasks["task-1"] = []taskTag{{TagID: "g", Human: true}}
			storeSidebarDoc(t, host, "ws-1", doc)
			snapshot := readMultiTagSnapshot(t, p)
			var decoded string
			require.NoError(t, json.Unmarshal([]byte(snapshot.Groups[0].Label), &decoded))
			require.Equal(t, name, decoded)
		})
	}
}

func TestSidebarGroupingEscapingRespectsActualLabelByteBounds(t *testing.T) {
	for _, tc := range []struct {
		action  string
		slashes int
		valid   bool
	}{
		{"sidebar-tags", 128, true}, {"sidebar-tags", 129, false},
		{"sidebar-multi-tags", 127, true}, {"sidebar-multi-tags", 128, false},
	} {
		p, host := newAgentTagTestPlugin()
		doc := newTagDoc()
		doc.Tags = []sharedTag{{ID: "g", Name: strings.Repeat(`\`, tc.slashes), Color: "#abc"}}
		doc.Tasks["task-1"] = []taskTag{{TagID: "g", Human: true}}
		storeSidebarDoc(t, host, "ws-1", doc)
		response, err := multiTagResult(t, p, tc.action)
		if !tc.valid {
			require.Error(t, err)
			require.Nil(t, response)
			continue
		}
		require.NoError(t, err)
		snapshot := readSidebarActionSnapshot(t, p, "ws-1", tc.action)
		require.Len(t, snapshot.Groups[0].Label, 256)
	}
}

func TestSidebarMultiTagDoesNotApplyAnUnusedLabelExpansionLimit(t *testing.T) {
	p, host := newAgentTagTestPlugin()
	doc := newTagDoc()
	doc.Tags = []sharedTag{{ID: "used", Name: "Used", Color: "#abc"}, {ID: "unused", Name: "A" + strings.Repeat("\x00", 60), Color: "#abc"}}
	doc.Tasks["task-1"] = []taskTag{{TagID: "used", Human: true}}
	storeSidebarDoc(t, host, "ws-1", doc)
	snapshot := readMultiTagSnapshot(t, p)
	require.Equal(t, map[string][]string{`"Used"`: {"task-1"}}, tasksByLabel(t, snapshot))
	response, err := multiTagResult(t, p, "sidebar-tags")
	require.Error(t, err)
	require.Nil(t, response)
}

func TestSidebarGroupingAcceptsNELOnlyNamesFromTheWriter(t *testing.T) {
	p, host := newAgentTagTestPlugin()
	rejected, err := p.HandleAction(t.Context(), actionReq("tag-create", []byte(`{"name":" \t\ufeff\r\n","color":"#abc"}`)))
	require.Error(t, err)
	require.Nil(t, rejected)
	created, err := p.HandleAction(t.Context(), actionReq("tag-create", []byte(`{"name":"\u0085","color":"#abc"}`)))
	require.NoError(t, err)
	tag := decodeActionBody(t, created)["tags"].([]any)[0].(map[string]any)
	id := tag["id"].(string)
	require.Equal(t, "\u0085", tag["name"])
	_, err = p.HandleAction(t.Context(), actionReq("task-tag-add", []byte(`{"tagId":"`+id+`"}`)))
	require.NoError(t, err)
	_, sets, deletes := host.stateCallCounts()
	single := readSidebarSnapshot(t, p, "ws-1")
	multi := readMultiTagSnapshot(t, p)
	require.Len(t, single.Groups, 1)
	require.Len(t, multi.Groups, 1)
	require.Equal(t, id, single.Groups[0].ID)
	require.Equal(t, `\u0085`, single.Groups[0].Label)
	require.Equal(t, `"\u0085"`, multi.Groups[0].Label)
	require.Equal(t, single, readSidebarSnapshot(t, p, "ws-1"))
	require.Equal(t, multi, readMultiTagSnapshot(t, p))
	_, afterSets, afterDeletes := host.stateCallCounts()
	require.Equal(t, sets, afterSets)
	require.Equal(t, deletes, afterDeletes)
}
