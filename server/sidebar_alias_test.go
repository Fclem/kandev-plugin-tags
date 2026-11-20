package main

import (
	"fmt"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestSidebarGroupingRejectsCaseFoldAliases(t *testing.T) {
	for _, action := range sidebarGroupingActions {
		for _, version := range []float64{1, 2} {
			for _, count := range []int{1, 10001} {
				t.Run(fmt.Sprintf("%s/v%v/%d", action, version, count), func(t *testing.T) {
					p, host := newAgentTagTestPlugin()
					entries := make([]any, count)
					for i := range entries {
						if version == 1 {
							entries[i] = map[string]any{"tag": "Ready"}
						} else {
							entries[i] = map[string]any{"tag_id": "g", "human": true}
						}
					}
					raw := map[string]any{"version": version, "tasks": map[string]any{}, "Tasks": map[string]any{"task-1": entries}}
					if version == 2 {
						raw["tags"] = []any{map[string]any{"id": "g", "name": "Ready", "color": "#123456"}}
					}
					require.NoError(t, host.SetState(t.Context(), "workspace", "ws-1", tagStateKey, raw))
					_, sets, deletes := host.stateCallCounts()
					req := actionReq(action, []byte(`{}`))
					req.Context.WorkspaceID = "ws-1"
					response, err := p.HandleAction(t.Context(), req)
					require.Error(t, err)
					require.Nil(t, response)
					_, afterSets, afterDeletes := host.stateCallCounts()
					require.Equal(t, sets, afterSets)
					require.Equal(t, deletes, afterDeletes)
				})
			}
		}
	}
}

func TestSidebarGroupingRejectsNestedClassificationAliases(t *testing.T) {
	for _, action := range sidebarGroupingActions {
		for _, alias := range []string{"Human", "TAG_ID", "Name", "ID", "Color"} {
			t.Run(action+"/"+alias, func(t *testing.T) {
				p, host := newAgentTagTestPlugin()
				definition := map[string]any{"id": "g", "name": "Ready", "color": "#123456"}
				application := map[string]any{"tag_id": "g", "human": false}
				if alias == "Human" {
					application[alias] = true
				} else if alias == "TAG_ID" {
					application[alias] = "hidden"
				} else {
					definition[alias] = "hidden"
				}
				raw := map[string]any{"version": float64(2), "tags": []any{definition}, "tasks": map[string]any{"task-1": []any{application}}}
				require.NoError(t, host.SetState(t.Context(), "workspace", "ws-1", tagStateKey, raw))
				_, sets, deletes := host.stateCallCounts()
				req := actionReq(action, []byte(`{}`))
				req.Context.WorkspaceID = "ws-1"
				response, err := p.HandleAction(t.Context(), req)
				require.Error(t, err)
				require.Nil(t, response)
				_, afterSets, afterDeletes := host.stateCallCounts()
				require.Equal(t, sets, afterSets)
				require.Equal(t, deletes, afterDeletes)
			})
		}
	}
}
