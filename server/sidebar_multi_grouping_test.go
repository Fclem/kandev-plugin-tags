package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/kdlbs/kandev/pluginsdk"
	"github.com/stretchr/testify/require"
)

const multiTagAction = "sidebar-multi-tags"

// expectedSetID is the test's independent oracle for the documented identity:
// "set-" plus the SHA-256 hex digest of the JSON array of sorted member tag IDs.
// Callers pass that JSON text literally.
func expectedSetID(memberJSON string) string {
	sum := sha256.Sum256([]byte(memberJSON))
	return "set-" + hex.EncodeToString(sum[:])
}

func readMultiTagSnapshot(t *testing.T, p *tagsPlugin) *pluginsdk.SidebarGroupingSnapshot {
	t.Helper()
	return readSidebarActionSnapshot(t, p, "ws-1", multiTagAction)
}

// tasksByLabel maps each combination heading to the sorted task IDs filed under it.
func tasksByLabel(t *testing.T, snapshot *pluginsdk.SidebarGroupingSnapshot) map[string][]string {
	t.Helper()
	labels := make(map[string]string, len(snapshot.Groups))
	for _, group := range snapshot.Groups {
		labels[group.ID] = group.Label
	}
	out := make(map[string][]string, len(snapshot.Groups))
	for _, pair := range snapshot.Memberships {
		label, ok := labels[pair.GroupID]
		require.True(t, ok, "membership references an undeclared value")
		out[label] = append(out[label], pair.TaskID)
	}
	for _, tasks := range out {
		slices.Sort(tasks)
	}
	return out
}

func multiTagResult(t *testing.T, p *tagsPlugin, action string) (*pluginsdk.PluginActionResponse, error) {
	t.Helper()
	return p.HandleAction(t.Context(), actionReq(action, nil))
}

// @covers AC-UI-SIDEBAR-MULTI-TAG-001.2
func TestSidebarMultiTagPowersetMemberships(t *testing.T) {
	p, host := newAgentTagTestPlugin()
	doc := newTagDoc()
	doc.Tags = []sharedTag{
		{ID: "c", Name: "Gamma", Color: "#333"},
		{ID: "a", Name: "Alpha", Color: "#111111"},
		{ID: "unused", Name: "Unused", Color: "#444444"},
		{ID: "b", Name: "Beta", Color: "#222222"},
	}
	doc.Tasks["abc"] = []taskTag{{TagID: "c", Human: true, Agent: true}, {TagID: "a", Human: true}, {TagID: "b", Agent: true}, {TagID: "a", Agent: true}}
	doc.Tasks["ab"] = []taskTag{{TagID: "b", Human: true}, {TagID: "a", Human: true}}
	doc.Tasks["bc"] = []taskTag{{TagID: "c", Agent: true}, {TagID: "b", Human: true}}
	doc.Tasks["with-deleted"] = []taskTag{{TagID: "gone", Human: true}, {TagID: "a", Human: true}, {TagID: "b"}}
	doc.Tasks["deleted-only"] = []taskTag{{TagID: "gone", Human: true}}
	doc.Tasks["inactive"] = []taskTag{{TagID: "a"}}
	doc.Tasks["empty"] = []taskTag{}
	storeSidebarDoc(t, host, "ws-1", doc)
	_, setsBefore, deletesBefore := host.stateCallCounts()

	snapshot := readMultiTagSnapshot(t, p)

	require.Equal(t, map[string][]string{
		`"Alpha"`:                    {"ab", "abc", "with-deleted"},
		`"Beta"`:                     {"ab", "abc", "bc"},
		`"Gamma"`:                    {"abc", "bc"},
		`"Alpha" + "Beta"`:           {"ab", "abc"},
		`"Alpha" + "Gamma"`:          {"abc"},
		`"Beta" + "Gamma"`:           {"abc", "bc"},
		`"Alpha" + "Beta" + "Gamma"`: {"abc"},
	}, tasksByLabel(t, snapshot))
	require.Len(t, snapshot.Groups, 7, "unused catalog tags and unobserved subsets create no values")
	require.Len(t, snapshot.Memberships, 7+3+3+1)
	_, setsAfter, deletesAfter := host.stateCallCounts()
	require.Equal(t, setsBefore, setsAfter)
	require.Equal(t, deletesBefore, deletesAfter)

	t.Run("legacy v1 applications without migration writes", func(t *testing.T) {
		p, host := newAgentTagTestPlugin()
		raw := map[string]any{"version": 1, "tasks": map[string]any{
			"legacy": []any{map[string]any{"tag": "ready"}, map[string]any{"tag": "blocked"}, map[string]any{"tag": "ready"}},
		}}
		require.NoError(t, host.SetState(t.Context(), "workspace", "ws-1", tagStateKey, raw))
		_, setsBefore, deletesBefore := host.stateCallCounts()
		require.Equal(t, map[string][]string{
			`"Blocked"`:           {"legacy"},
			`"Ready"`:             {"legacy"},
			`"Blocked" + "Ready"`: {"legacy"},
		}, tasksByLabel(t, readMultiTagSnapshot(t, p)))
		_, setsAfter, deletesAfter := host.stateCallCounts()
		require.Equal(t, setsBefore, setsAfter)
		require.Equal(t, deletesBefore, deletesAfter)
	})
}

func TestSidebarMultiTagIgnoresUnobservedCatalogSize(t *testing.T) {
	p, host := newAgentTagTestPlugin()
	doc := newTagDoc()
	for i := 0; i <= pluginsdk.MaxSidebarGroupingValues; i++ {
		doc.Tags = append(doc.Tags, sharedTag{ID: fmt.Sprintf("tag-%d", i), Name: fmt.Sprintf("Tag %d", i), Color: "#123456"})
	}
	doc.Tasks["task"] = []taskTag{{TagID: "tag-0", Human: true}}
	storeSidebarDoc(t, host, "ws-1", doc)
	snapshot := readMultiTagSnapshot(t, p)
	require.Len(t, snapshot.Groups, 1)
	require.Equal(t, map[string][]string{`"Tag 0"`: {"task"}}, tasksByLabel(t, snapshot))
}

// @covers AC-UI-SIDEBAR-MULTI-TAG-001.3
func TestSidebarMultiTagStableIdentityAndRevision(t *testing.T) {
	p, host := newAgentTagTestPlugin()
	backend := sharedTag{ID: "x", Name: "Backend", Color: "#00aaff", Owner: ownerHuman, CreatedAt: "2026-01-01T00:00:00Z"}
	frontend := sharedTag{ID: "y", Name: "Frontend", Color: "#ff8800", Owner: ownerAgent}
	doc := newTagDoc()
	doc.Tags = []sharedTag{backend, frontend}
	doc.Tasks["t1"] = []taskTag{{TagID: "y", Human: true}, {TagID: "x", Agent: true, Note: "first"}}
	doc.Tasks["t2"] = []taskTag{{TagID: "x", Human: true}}
	storeSidebarDoc(t, host, "ws-1", doc)
	read := func() *pluginsdk.SidebarGroupingSnapshot {
		t.Helper()
		_, setsBefore, deletesBefore := host.stateCallCounts()
		snapshot := readMultiTagSnapshot(t, p)
		_, setsAfter, deletesAfter := host.stateCallCounts()
		require.Equal(t, setsBefore, setsAfter)
		require.Equal(t, deletesBefore, deletesAfter)
		return snapshot
	}
	onlyX, onlyY, both := expectedSetID(`["x"]`), expectedSetID(`["y"]`), expectedSetID(`["x","y"]`)

	original := read()
	require.ElementsMatch(t, []pluginsdk.SidebarGroupingValue{
		{ID: onlyX, Label: `"Backend"`, Color: "#00aaff"},
		{ID: onlyY, Label: `"Frontend"`, Color: "#ff8800"},
		{ID: both, Label: `"Backend" + "Frontend"`},
	}, original.Groups)
	require.ElementsMatch(t, []pluginsdk.SidebarGroupingMembership{
		{TaskID: "t1", GroupID: onlyX}, {TaskID: "t1", GroupID: onlyY}, {TaskID: "t1", GroupID: both},
		{TaskID: "t2", GroupID: onlyX},
	}, original.Memberships)

	permuted := newTagDoc()
	permuted.Tags = []sharedTag{{ID: "y", Name: "Frontend", Color: "#F80", Owner: ownerHuman, UpdatedAt: "2099-01-01T00:00:00Z"}, {ID: "x", Name: "Backend", Color: "#00AAFF"}}
	permuted.Tasks["t2"] = []taskTag{{TagID: "x", Agent: true, Human: true, SessionID: "s", UpdatedAt: "2099-01-01T00:00:00Z"}}
	permuted.Tasks["t1"] = []taskTag{{TagID: "x", Human: true, Note: "different"}, {TagID: "y", Agent: true}, {TagID: "x", Agent: true}}
	storeSidebarDoc(t, host, "ws-1", permuted)
	reordered := read()
	require.Equal(t, original.Revision, reordered.Revision, "order, ownership, notes and timestamps are not classification")
	require.ElementsMatch(t, original.Groups, reordered.Groups)
	require.ElementsMatch(t, original.Memberships, reordered.Memberships)

	doc.Tags[1].Name = `Front "end" + UI`
	storeSidebarDoc(t, host, "ws-1", doc)
	renamed := read()
	require.NotEqual(t, original.Revision, renamed.Revision)
	require.ElementsMatch(t, []pluginsdk.SidebarGroupingValue{
		{ID: onlyX, Label: `"Backend"`, Color: "#00aaff"},
		{ID: onlyY, Label: `"Front \"end\" + UI"`, Color: "#ff8800"},
		{ID: both, Label: `"Backend" + "Front \"end\" + UI"`},
	}, renamed.Groups)

	doc.Tags[0].Color = "#123"
	storeSidebarDoc(t, host, "ws-1", doc)
	recolored := read()
	require.NotEqual(t, renamed.Revision, recolored.Revision)
	require.Contains(t, recolored.Groups, pluginsdk.SidebarGroupingValue{ID: onlyX, Label: `"Backend"`, Color: "#112233"})
	require.Contains(t, recolored.Groups, pluginsdk.SidebarGroupingValue{ID: both, Label: `"Backend" + "Front \"end\" + UI"`})

	doc.Tags = doc.Tags[1:]
	storeSidebarDoc(t, host, "ws-1", doc)
	deleted := read()
	require.NotEqual(t, recolored.Revision, deleted.Revision)
	require.Equal(t, []pluginsdk.SidebarGroupingValue{{ID: onlyY, Label: `"Front \"end\" + UI"`, Color: "#ff8800"}}, deleted.Groups)
	require.Equal(t, []pluginsdk.SidebarGroupingMembership{{TaskID: "t1", GroupID: onlyY}}, deleted.Memberships)

	doc.Tags = []sharedTag{frontend, backend}
	storeSidebarDoc(t, host, "ws-1", doc)
	restored := read()
	require.Equal(t, original.Revision, restored.Revision, "reapplying the same classification restores the same content revision")
	require.ElementsMatch(t, original.Groups, restored.Groups)

	t.Run("punctuation in IDs and names never collides with a combination", func(t *testing.T) {
		p, host := newAgentTagTestPlugin()
		doc := newTagDoc()
		doc.Tags = []sharedTag{
			{ID: "a", Name: "Alpha", Color: "#111111"},
			{ID: "b", Name: "Beta", Color: "#222222"},
			{ID: "a,b", Name: `Alpha" + "Beta`, Color: "#333333"},
		}
		doc.Tasks["pair"] = []taskTag{{TagID: "a", Human: true}, {TagID: "b", Human: true}}
		doc.Tasks["single"] = []taskTag{{TagID: "a,b", Human: true}}
		storeSidebarDoc(t, host, "ws-1", doc)
		snapshot := readMultiTagSnapshot(t, p)
		require.Contains(t, snapshot.Memberships, pluginsdk.SidebarGroupingMembership{TaskID: "pair", GroupID: expectedSetID(`["a","b"]`)})
		require.Contains(t, snapshot.Memberships, pluginsdk.SidebarGroupingMembership{TaskID: "single", GroupID: expectedSetID(`["a,b"]`)})
		labels := tasksByLabel(t, snapshot)
		require.Equal(t, []string{"pair"}, labels[`"Alpha" + "Beta"`])
		require.Equal(t, []string{"single"}, labels[`"Alpha\" + \"Beta"`])
	})
}

func storeTagsOnTask(doc *tagDoc, taskID string, count int) {
	for i := range count {
		id := fmt.Sprintf("tag-%03d", i)
		if findTag(doc.Tags, id) == nil {
			doc.Tags = append(doc.Tags, sharedTag{ID: id, Name: "Tag " + id, Color: "#123456"})
		}
		doc.Tasks[taskID] = append(doc.Tasks[taskID], taskTag{TagID: id, Human: true})
	}
}

// @covers AC-UI-SIDEBAR-MULTI-TAG-001.5
func TestSidebarMultiTagExpansionBounds(t *testing.T) {
	type outcome struct {
		groups, memberships int
	}
	cases := []struct {
		name  string
		build func(doc *tagDoc)
		// want is nil when the complete combination snapshot must be refused.
		want *outcome
	}{
		{"nine tags on one task", func(doc *tagDoc) { storeTagsOnTask(doc, "task", 9) }, &outcome{511, 511}},
		{"ten tags on one task", func(doc *tagDoc) { storeTagsOnTask(doc, "task", 10) }, nil},
		{"sixty-four tags on one task", func(doc *tagDoc) { storeTagsOnTask(doc, "task", 64) }, nil},
		{"distinct values at bound", func(doc *tagDoc) { storeDistinctPairs(doc, 950) }, &outcome{1000, 950 * 3}},
		{"distinct values over bound", func(doc *tagDoc) { storeDistinctPairs(doc, 951) }, nil},
		{"memberships at bound", func(doc *tagDoc) { storeRepeatedPairs(doc, 3333, 1) }, &outcome{3, pluginsdk.MaxSidebarGroupingMemberships}},
		{"memberships over bound", func(doc *tagDoc) { storeRepeatedPairs(doc, 3333, 2) }, nil},
		{"quoted label at 256 bytes", func(doc *tagDoc) { storeLabelPair(doc, strings.Repeat("é", 63)+"a") }, &outcome{3, 3}},
		{"quoted label over 256 bytes", func(doc *tagDoc) { storeLabelPair(doc, strings.Repeat("é", 63)+"ab") }, nil},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			p, host := newAgentTagTestPlugin()
			doc := newTagDoc()
			testCase.build(&doc)
			storeSidebarDoc(t, host, "ws-1", doc)
			response, err := multiTagResult(t, p, multiTagAction)
			if testCase.want == nil {
				require.Error(t, err)
				require.Nil(t, response)
				// Combination-only overflow leaves the single Tag grouping usable.
				readSidebarSnapshot(t, p, "ws-1")
				return
			}
			require.NoError(t, err)
			snapshot, err := pluginsdk.DecodeSidebarGroupingSnapshot(response.Body)
			require.NoError(t, err)
			require.Equal(t, *testCase.want, outcome{len(snapshot.Groups), len(snapshot.Memberships)})
		})
	}
}

// storeDistinctPairs files each task under a different two-tag set drawn from
// fifty tags, so every task adds one new compound value plus shared singletons.
func storeDistinctPairs(doc *tagDoc, count int) {
	for i := range 50 {
		doc.Tags = append(doc.Tags, sharedTag{ID: fmt.Sprintf("tag-%02d", i), Name: fmt.Sprintf("Tag %02d", i), Color: "#123456"})
	}
	added := 0
	for i := 0; i < 50 && added < count; i++ {
		for j := i + 1; j < 50 && added < count; j++ {
			doc.Tasks[fmt.Sprintf("task-%04d", added)] = []taskTag{{TagID: doc.Tags[i].ID, Human: true}, {TagID: doc.Tags[j].ID, Human: true}}
			added++
		}
	}
}

// storeRepeatedPairs gives many tasks the same two-tag set (three pairs each)
// plus single-tag tasks, so only the membership budget is approached. Task IDs
// stay short: 10,000 memberships with 68-byte value IDs nearly fill 1 MiB.
func storeRepeatedPairs(doc *tagDoc, pairTasks, singleTasks int) {
	doc.Tags = []sharedTag{{ID: "a", Name: "Alpha", Color: "#111111"}, {ID: "b", Name: "Beta", Color: "#222222"}}
	for i := range pairTasks {
		doc.Tasks[fmt.Sprintf("p%04d", i)] = []taskTag{{TagID: "a", Human: true}, {TagID: "b", Agent: true}}
	}
	for i := range singleTasks {
		doc.Tasks[fmt.Sprintf("s%04d", i)] = []taskTag{{TagID: "a", Human: true}}
	}
}

// storeLabelPair applies two tags whose combined quoted label is
// `"<first>" + "<second>"`. The first name contains a quote that is escaped in
// the label, so it occupies 2+122 bytes; the separator adds 3.
func storeLabelPair(doc *tagDoc, second string) {
	first := strings.Repeat("é", 60) + `"`
	doc.Tags = []sharedTag{{ID: "a", Name: first, Color: "#111111"}, {ID: "b", Name: second, Color: "#222222"}}
	doc.Tasks["task"] = []taskTag{{TagID: "a", Human: true}, {TagID: "b", Human: true}}
}

// @covers AC-UI-SIDEBAR-MULTI-TAG-001.5
func TestSidebarMultiTagEnforcesEncodedResponseBound(t *testing.T) {
	const tasks, baseLength, maxLength = 3400, 200, 256
	build := func(extra int) tagDoc {
		doc := newTagDoc()
		doc.Tags = []sharedTag{{ID: "g", Name: "Ready", Color: "#123456"}}
		for i := range tasks {
			grow := min(extra, maxLength-baseLength)
			extra -= grow
			doc.Tasks[fmt.Sprintf("%0*d", baseLength+grow, i)] = []taskTag{{TagID: "g", Human: true}}
		}
		if extra != 0 {
			panic("response bound fixture cannot absorb the requested growth")
		}
		return doc
	}
	p, host := newAgentTagTestPlugin()
	storeSidebarDoc(t, host, "ws-1", build(0))
	base, err := multiTagResult(t, p, multiTagAction)
	require.NoError(t, err)
	// Each extra ASCII task ID byte adds exactly one encoded response byte.
	deficit := pluginsdk.MaxSidebarGroupingResponseBytes - len(base.Body)
	require.Positive(t, deficit)

	storeSidebarDoc(t, host, "ws-1", build(deficit))
	exact, err := multiTagResult(t, p, multiTagAction)
	require.NoError(t, err)
	require.Len(t, exact.Body, pluginsdk.MaxSidebarGroupingResponseBytes)
	_, err = pluginsdk.DecodeSidebarGroupingSnapshot(exact.Body)
	require.NoError(t, err)

	storeSidebarDoc(t, host, "ws-1", build(deficit+1))
	over, err := multiTagResult(t, p, multiTagAction)
	require.Error(t, err)
	require.Nil(t, over)
	tag, err := multiTagResult(t, p, "sidebar-tags")
	require.NoError(t, err, "the shorter singleton Tag snapshot remains available")
	require.LessOrEqual(t, len(tag.Body), pluginsdk.MaxSidebarGroupingResponseBytes)
}

type cancelOnReadHost struct {
	*fakeHost
	cancel context.CancelFunc
}

func (h cancelOnReadHost) GetState(ctx context.Context, scope, scopeID, key string) (map[string]any, bool, error) {
	raw, found, err := h.fakeHost.GetState(ctx, scope, scopeID, key)
	h.cancel()
	return raw, found, err
}

// @covers AC-UI-SIDEBAR-MULTI-TAG-001.5
func TestSidebarMultiTagCancellationReturnsNoSnapshot(t *testing.T) {
	p, host := newAgentTagTestPlugin()
	doc := newTagDoc()
	storeTagsOnTask(&doc, "task", 3)
	storeSidebarDoc(t, host, "ws-1", doc)

	ctx, cancel := context.WithCancel(t.Context())
	p.SetHost(cancelOnReadHost{fakeHost: host, cancel: cancel})
	response, err := p.HandleAction(ctx, actionReq(multiTagAction, nil))
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, response)

	p.SetHost(host)
	gets, _, _ := host.stateCallCounts()
	response, err = p.HandleAction(ctx, actionReq(multiTagAction, nil))
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, response)
	getsAfter, _, _ := host.stateCallCounts()
	require.Equal(t, gets, getsAfter, "an already-canceled request does not read shared state")
}
