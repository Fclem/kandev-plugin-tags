package main

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSidebarGroupingProjectsLegitimateControlNames(t *testing.T) {
	p, host := newAgentTagTestPlugin()
	created, err := p.HandleAction(t.Context(), actionReq("tag-create", []byte(`{"name":"Ready\tNow","color":"#abc"}`)))
	require.NoError(t, err)
	tag := decodeActionBody(t, created)["tags"].([]any)[0].(map[string]any)
	id := tag["id"].(string)
	require.Equal(t, "Ready\tNow", tag["name"])
	_, err = p.HandleAction(t.Context(), actionReq("task-tag-add", []byte(`{"tagId":"`+id+`"}`)))
	require.NoError(t, err)
	for _, name := range []string{"Ready\tNow", "Ready\aNow"} {
		payload, _ := json.Marshal(map[string]string{"id": id, "name": name})
		_, err = p.HandleAction(t.Context(), actionReq("tag-update", payload))
		require.NoError(t, err)
		_, sets, deletes := host.stateCallCounts()
		escaped := strconv.QuoteRune([]rune(name)[5])
		label := "Ready" + escaped[1:len(escaped)-1] + "Now"
		single := readSidebarActionSnapshot(t, p, "ws-1", "sidebar-tags")
		require.Equal(t, id, single.Groups[0].ID)
		require.Equal(t, label, single.Groups[0].Label)
		multi := readSidebarActionSnapshot(t, p, "ws-1", "sidebar-multi-tags")
		require.Equal(t, strconv.Quote(label), multi.Groups[0].Label)
		_, afterSets, afterDeletes := host.stateCallCounts()
		require.Equal(t, sets, afterSets)
		require.Equal(t, deletes, afterDeletes)
	}
}

func TestSidebarGroupingReadsHistoricalCatalogControlName(t *testing.T) {
	p, host := newAgentTagTestPlugin()
	doc := newTagDoc()
	doc.Tags = []sharedTag{{ID: "historical", Name: "Ready\tNow", Color: "#abc"}}
	doc.Tasks["task-1"] = []taskTag{{TagID: "historical", Agent: true}}
	storeSidebarDoc(t, host, "ws-1", doc)
	require.Equal(t, "Ready\\tNow", readSidebarSnapshot(t, p, "ws-1").Groups[0].Label)
	require.NotEmpty(t, readSidebarActionSnapshot(t, p, "ws-1", "sidebar-multi-tags").Groups)
}
