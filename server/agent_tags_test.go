package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/kandev/kandev/pkg/pluginsdk"
	"github.com/stretchr/testify/require"
)

func newAgentTagTestPlugin() (*tagsPlugin, *fakeHost) {
	host := &fakeHost{}
	p := &tagsPlugin{}
	p.SetHost(host)
	return p, host
}

func agentToolReq(name string, args map[string]any) *pluginsdk.AgentToolRequest {
	return &pluginsdk.AgentToolRequest{Name: name, Arguments: args, Context: pluginsdk.AgentToolContext{TaskID: "task-1", SessionID: "session-1", WorkspaceID: "ws-1", Surface: "kanban-task"}}
}
func storedTagDoc(t *testing.T, host *fakeHost, workspaceID string) tagDoc {
	t.Helper()
	raw, found, err := host.GetState(context.Background(), "workspace", workspaceID, tagStateKey)
	require.NoError(t, err)
	require.True(t, found)
	doc, err := decodeTagDoc(raw)
	require.NoError(t, err)
	return doc
}
func agentCreate(t *testing.T, p *tagsPlugin, name string) string {
	t.Helper()
	result, err := p.InvokeAgentTool(context.Background(), agentToolReq("create_tag", map[string]any{"name": name, "color": "#2563eb"}))
	require.NoError(t, err)
	require.False(t, result.IsError, result.Text)
	catalog := result.StructuredContent["catalog"].([]any)
	for _, raw := range catalog {
		tag := raw.(map[string]any)
		if tag["name"] == name {
			return tag["id"].(string)
		}
	}
	t.Fatal("created tag missing from tool result")
	return ""
}

func TestAgentCanCreateApplyEditAndDeleteOwnSharedTag(t *testing.T) {
	p, host := newAgentTagTestPlugin()
	id := agentCreate(t, p, "Waiting on API")

	result, err := p.InvokeAgentTool(context.Background(), agentToolReq("add_tag", map[string]any{"tag_id": id, "note": "credentials requested"}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	doc := storedTagDoc(t, host, "ws-1")
	require.Equal(t, ownerAgent, doc.Tags[0].Owner)
	require.Equal(t, true, doc.Tasks["task-1"][0].Agent)
	require.Equal(t, "credentials requested", doc.Tasks["task-1"][0].Note)

	result, err = p.InvokeAgentTool(context.Background(), agentToolReq("update_tag", map[string]any{"tag_id": id, "name": "Waiting for API", "color": "#f59e0b"}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	doc = storedTagDoc(t, host, "ws-1")
	require.Equal(t, "Waiting for API", doc.Tags[0].Name)
	require.Equal(t, "#f59e0b", doc.Tags[0].Color)

	result, err = p.InvokeAgentTool(context.Background(), agentToolReq("delete_tag", map[string]any{"tag_id": id}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	doc = storedTagDoc(t, host, "ws-1")
	require.Empty(t, doc.Tags)
	require.Empty(t, doc.Tasks)
}

func TestAgentCannotManageHumanOwnedTagAndHumanApplicationSurvivesAgentRemoval(t *testing.T) {
	p, host := newAgentTagTestPlugin()
	_, err := p.HandleAction(context.Background(), actionReq("tag-create", []byte(`{"name":"Human priority","color":"#ef4444"}`)))
	require.NoError(t, err)
	doc := storedTagDoc(t, host, "ws-1")
	humanID := doc.Tags[0].ID

	result, err := p.InvokeAgentTool(context.Background(), agentToolReq("add_tag", map[string]any{"tag_id": humanID}))
	require.NoError(t, err)
	require.True(t, result.IsError)
	require.Contains(t, result.Text, "agent-created")

	agentID := agentCreate(t, p, "Agent marker")
	_, err = p.InvokeAgentTool(context.Background(), agentToolReq("add_tag", map[string]any{"tag_id": agentID}))
	require.NoError(t, err)
	_, err = p.HandleAction(context.Background(), actionReq("task-tag-add", []byte(`{"tagId":"`+agentID+`"}`)))
	require.NoError(t, err)
	_, err = p.InvokeAgentTool(context.Background(), agentToolReq("remove_tag", map[string]any{"tag_id": agentID}))
	require.NoError(t, err)
	doc = storedTagDoc(t, host, "ws-1")
	entry := doc.Tasks["task-1"][0]
	require.False(t, entry.Agent)
	require.True(t, entry.Human)
}

func TestAgentToolTruncatesNoteAndValidatesContext(t *testing.T) {
	p, host := newAgentTagTestPlugin()
	id := agentCreate(t, p, "Long note")
	long := strings.Repeat("界", 205)
	result, err := p.InvokeAgentTool(context.Background(), agentToolReq("add_tag", map[string]any{"tag_id": id, "note": long}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	require.Len(t, []rune(storedTagDoc(t, host, "ws-1").Tasks["task-1"][0].Note), maxTagNoteRunes)

	req := agentToolReq("list_tags", nil)
	req.Context.WorkspaceID = ""
	result, err = p.InvokeAgentTool(context.Background(), req)
	require.NoError(t, err)
	require.True(t, result.IsError)
	require.Contains(t, result.Text, "workspace_id")
}

func TestSharedTagsSurviveRuntimeReplacementAndCompatibleRollback(t *testing.T) {
	require.Equal(t, "agent-tags", tagStateKey, "changing the state key orphans every existing workspace catalog")

	host := &fakeHost{}
	current := &tagsPlugin{}
	current.SetHost(host)

	// Build a mixed catalog and provenance-rich applications through the real
	// human action and agent tool surfaces.
	_, err := current.HandleAction(context.Background(), actionReq("tag-create", []byte(`{"name":"Human priority","color":"#ef4444"}`)))
	require.NoError(t, err)
	humanID := storedTagDoc(t, host, "ws-1").Tags[0].ID
	agentID := agentCreate(t, current, "Waiting on API")

	result, err := current.InvokeAgentTool(context.Background(), agentToolReq("add_tag", map[string]any{
		"tag_id": agentID,
		"note":   "credentials requested",
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	_, err = current.HandleAction(context.Background(), actionReq("task-tag-add", []byte(`{"tagId":"`+agentID+`"}`)))
	require.NoError(t, err)
	_, err = current.HandleAction(context.Background(), actionReq("task-tag-add", []byte(`{"tagId":"`+humanID+`"}`)))
	require.NoError(t, err)
	secondTask := actionReq("task-tag-add", []byte(`{"tagId":"`+humanID+`"}`))
	secondTask.Context.TaskID = "task-2"
	_, err = current.HandleAction(context.Background(), secondTask)
	require.NoError(t, err)

	beforeReplacement := storedTagDoc(t, host, "ws-1")
	_, setsBeforeRead, deletesBeforeRead := host.stateCallCounts()

	// Kandev replaces the subprocess but retains the same host state namespace
	// for an in-place update. A fresh plugin instance must read, not initialize
	// over, the complete prior document.
	replacement := &tagsPlugin{}
	replacement.SetHost(host)
	response, err := replacement.HandleAction(context.Background(), actionReq("shared-tags", nil))
	require.NoError(t, err)
	require.NotEmpty(t, response.Body)
	afterReplacement, err := replacement.readTagDoc(context.Background(), "ws-1")
	require.NoError(t, err)
	require.Equal(t, beforeReplacement, afterReplacement)
	_, setsAfterRead, deletesAfterRead := host.stateCallCounts()
	require.Equal(t, setsBeforeRead, setsAfterRead, "replacement startup/read must not overwrite state")
	require.Equal(t, deletesBeforeRead, deletesAfterRead, "replacement startup/read must not delete state")

	// Mutate through the replacement, then model a rollback by binding another
	// fresh runtime to the same namespace. Every field written by the newer
	// compatible runtime must remain readable by the previous one.
	result, err = replacement.InvokeAgentTool(context.Background(), agentToolReq("update_tag", map[string]any{
		"tag_id": agentID,
		"name":   "Waiting for API",
		"color":  "#f59e0b",
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	agentOnSecond := agentToolReq("add_tag", map[string]any{"tag_id": agentID, "note": "second task note"})
	agentOnSecond.Context.TaskID = "task-2"
	agentOnSecond.Context.SessionID = "session-2"
	result, err = replacement.InvokeAgentTool(context.Background(), agentOnSecond)
	require.NoError(t, err)
	require.False(t, result.IsError)

	expectedAfterRollback := storedTagDoc(t, host, "ws-1")
	rollback := &tagsPlugin{}
	rollback.SetHost(host)
	_, setsBeforeRollbackRead, deletesBeforeRollbackRead := host.stateCallCounts()
	afterRollback, err := rollback.readTagDoc(context.Background(), "ws-1")
	require.NoError(t, err)
	require.Equal(t, expectedAfterRollback, afterRollback)
	_, setsAfterRollbackRead, deletesAfterRollbackRead := host.stateCallCounts()
	require.Equal(t, setsBeforeRollbackRead, setsAfterRollbackRead)
	require.Equal(t, deletesBeforeRollbackRead, deletesAfterRollbackRead)
	require.Equal(t, "second task note", afterRollback.Tasks["task-2"][1].Note)
	require.Equal(t, "session-2", afterRollback.Tasks["task-2"][1].SessionID)
}

func TestLegacyAgentStatusDocumentMigratesOnNextWrite(t *testing.T) {
	p, host := newAgentTagTestPlugin()
	legacy := map[string]any{"version": 1, "tasks": map[string]any{"task-1": []any{map[string]any{"tag": "blocked", "note": "waiting", "updated_at": "2026-01-01T00:00:00Z"}}}}
	require.NoError(t, host.SetState(context.Background(), "workspace", "ws-1", tagStateKey, legacy))
	result, err := p.InvokeAgentTool(context.Background(), agentToolReq("list_tags", nil))
	require.NoError(t, err)
	require.False(t, result.IsError)
	require.Equal(t, "Blocked", result.StructuredContent["tags"].([]any)[0].(map[string]any)["name"])
	_, err = p.InvokeAgentTool(context.Background(), agentToolReq("create_tag", map[string]any{"name": "new"}))
	require.NoError(t, err)
	doc := storedTagDoc(t, host, "ws-1")
	require.Equal(t, 2, doc.Version)
	require.Len(t, doc.Tags, 2)
}

func TestMutationsAreSerializedAndRejectNewAgentTaskAtCap(t *testing.T) {
	p, host := newAgentTagTestPlugin()
	id := agentCreate(t, p, "Concurrent")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := agentToolReq("add_tag", map[string]any{"tag_id": id})
			req.Context.TaskID = fmt.Sprintf("task-%02d", i)
			_, err := p.InvokeAgentTool(context.Background(), req)
			require.NoError(t, err)
		}(i)
	}
	wg.Wait()
	require.Len(t, storedTagDoc(t, host, "ws-1").Tasks, 50)

	doc := newTagDoc()
	doc.Tags = []sharedTag{{ID: id, Name: "Concurrent", Color: defaultTagColor, Owner: ownerAgent}}
	for i := 0; i < tagTaskCap; i++ {
		doc.Tasks[fmt.Sprintf("old-%03d", i)] = []taskTag{{TagID: id, Agent: true, UpdatedAt: fmt.Sprintf("2026-01-01T00:00:%02dZ", i)}}
	}
	raw, err := encodeTagDoc(doc)
	require.NoError(t, err)
	require.NoError(t, host.SetState(context.Background(), "workspace", "ws-cap", tagStateKey, raw))
	req := agentToolReq("add_tag", map[string]any{"tag_id": id})
	req.Context.WorkspaceID = "ws-cap"
	req.Context.TaskID = "new"
	result, err := p.InvokeAgentTool(context.Background(), req)
	require.NoError(t, err)
	require.True(t, result.IsError)
	require.Equal(t, fmt.Sprintf("workspace tag task capacity is %d; remove a task tag before targeting a new task", tagTaskCap), result.Text)
	capDoc := storedTagDoc(t, host, "ws-cap")
	require.Len(t, capDoc.Tasks, tagTaskCap)
	require.Contains(t, capDoc.Tasks, "old-000")
	require.NotContains(t, capDoc.Tasks, "new")
}

// The optional task_id argument lets an agent (typically a coordinator) tag a
// card other than its own. Workspace scoping is unchanged: the target is a key
// inside the caller's own workspace document.
func TestAgentToolsTargetAnotherTaskWhenTaskIDSupplied(t *testing.T) {
	p, host := newAgentTagTestPlugin()
	id := agentCreate(t, p, "Needs review")

	result, err := p.InvokeAgentTool(context.Background(), agentToolReq("add_tag", map[string]any{"tag_id": id, "task_id": "task-other", "note": "queued behind #42"}))
	require.NoError(t, err)
	require.False(t, result.IsError, result.Text)

	doc := storedTagDoc(t, host, "ws-1")
	require.NotContains(t, doc.Tasks, "task-1", "caller's own task must be untouched")
	require.Len(t, doc.Tasks["task-other"], 1)
	require.True(t, doc.Tasks["task-other"][0].Agent)
	require.Equal(t, "queued behind #42", doc.Tasks["task-other"][0].Note)
	require.Equal(t, "session-1", doc.Tasks["task-other"][0].SessionID)

	// The result view reflects the target task, not the caller's.
	require.Len(t, result.StructuredContent["tags"].([]any), 1)
	require.Equal(t, "Needs review", result.StructuredContent["tags"].([]any)[0].(map[string]any)["name"])

	listed, err := p.InvokeAgentTool(context.Background(), agentToolReq("list_tags", map[string]any{"task_id": "task-other"}))
	require.NoError(t, err)
	require.False(t, listed.IsError, listed.Text)
	require.Len(t, listed.StructuredContent["tags"].([]any), 1)

	removed, err := p.InvokeAgentTool(context.Background(), agentToolReq("remove_tag", map[string]any{"tag_id": id, "task_id": "task-other"}))
	require.NoError(t, err)
	require.False(t, removed.IsError, removed.Text)
	require.NotContains(t, storedTagDoc(t, host, "ws-1").Tasks, "task-other")
	require.Empty(t, removed.StructuredContent["tags"].([]any))
}

// The compatibility guarantee: omitting task_id -- or passing it blank -- must
// behave exactly as before, acting on the calling agent's own task.
func TestAgentToolsFallBackToCallerTaskWhenTaskIDOmitted(t *testing.T) {
	p, host := newAgentTagTestPlugin()
	id := agentCreate(t, p, "Own card")

	_, err := p.InvokeAgentTool(context.Background(), agentToolReq("add_tag", map[string]any{"tag_id": id}))
	require.NoError(t, err)
	require.Len(t, storedTagDoc(t, host, "ws-1").Tasks["task-1"], 1)

	// A blank or whitespace-only argument is treated as absent rather than
	// creating an entry under an empty task key.
	_, err = p.InvokeAgentTool(context.Background(), agentToolReq("remove_tag", map[string]any{"tag_id": id, "task_id": "   "}))
	require.NoError(t, err)
	doc := storedTagDoc(t, host, "ws-1")
	require.NotContains(t, doc.Tasks, "task-1")
	require.NotContains(t, doc.Tasks, "")
	require.NotContains(t, doc.Tasks, "   ")

	blank, err := p.InvokeAgentTool(context.Background(), agentToolReq("add_tag", map[string]any{"tag_id": id, "task_id": ""}))
	require.NoError(t, err)
	require.False(t, blank.IsError, blank.Text)
	require.Len(t, storedTagDoc(t, host, "ws-1").Tasks["task-1"], 1)
}

// A task_id naming a card with no existing entries is accepted (decision A):
// the plugin has no platform client to validate against, and an inert entry is
// contained by workspace scoping and reaped by the existing eviction path.
func TestAgentToolsAcceptTargetTaskWithNoExistingEntries(t *testing.T) {
	p, host := newAgentTagTestPlugin()
	id := agentCreate(t, p, "Fresh")

	listed, err := p.InvokeAgentTool(context.Background(), agentToolReq("list_tags", map[string]any{"task_id": "never-seen"}))
	require.NoError(t, err)
	require.False(t, listed.IsError, listed.Text)
	require.Empty(t, listed.StructuredContent["tags"].([]any))
	require.Len(t, listed.StructuredContent["catalog"].([]any), 1)

	// Removing from a task that has no entries is a no-op, not an error.
	removed, err := p.InvokeAgentTool(context.Background(), agentToolReq("remove_tag", map[string]any{"tag_id": id, "task_id": "never-seen"}))
	require.NoError(t, err)
	require.False(t, removed.IsError, removed.Text)
	require.NotContains(t, storedTagDoc(t, host, "ws-1").Tasks, "never-seen")

	added, err := p.InvokeAgentTool(context.Background(), agentToolReq("add_tag", map[string]any{"tag_id": id, "task_id": "never-seen"}))
	require.NoError(t, err)
	require.False(t, added.IsError, added.Text)
	require.Len(t, storedTagDoc(t, host, "ws-1").Tasks["never-seen"], 1)
}

// Targeting another task must not become a way around agent ownership, and a
// human's application on the target task must survive an agent removal.
func TestAgentToolsPreserveOwnershipOnTargetedTask(t *testing.T) {
	p, host := newAgentTagTestPlugin()
	_, err := p.HandleAction(context.Background(), actionReq("tag-create", []byte(`{"name":"Human only","color":"#ef4444"}`)))
	require.NoError(t, err)
	humanID := storedTagDoc(t, host, "ws-1").Tags[0].ID

	result, err := p.InvokeAgentTool(context.Background(), agentToolReq("add_tag", map[string]any{"tag_id": humanID, "task_id": "task-other"}))
	require.NoError(t, err)
	require.True(t, result.IsError)
	require.Contains(t, result.Text, "agent-created")
	require.NotContains(t, storedTagDoc(t, host, "ws-1").Tasks, "task-other")

	// A human application on the target task, plus an agent application of the
	// same tag: removing the agent's leaves the human's intact.
	agentID := agentCreate(t, p, "Shared marker")
	_, err = p.InvokeAgentTool(context.Background(), agentToolReq("add_tag", map[string]any{"tag_id": agentID, "task_id": "task-other"}))
	require.NoError(t, err)
	doc := storedTagDoc(t, host, "ws-1")
	doc.Tasks["task-other"][0].Human = true
	raw, err := encodeTagDoc(doc)
	require.NoError(t, err)
	require.NoError(t, host.SetState(context.Background(), "workspace", "ws-1", tagStateKey, raw))

	_, err = p.InvokeAgentTool(context.Background(), agentToolReq("remove_tag", map[string]any{"tag_id": agentID, "task_id": "task-other"}))
	require.NoError(t, err)
	entry := storedTagDoc(t, host, "ws-1").Tasks["task-other"][0]
	require.False(t, entry.Agent, "agent application removed")
	require.True(t, entry.Human, "human application survives")
}

// Catalog tools stay workspace-scoped and unaffected by any task targeting
// (decision D1: no task_id on update_tag/delete_tag), and delete still
// cascades across every task including ones an agent targeted remotely.
func TestCatalogToolsIgnoreTaskTargetingAndStillCascade(t *testing.T) {
	p, host := newAgentTagTestPlugin()
	id := agentCreate(t, p, "Cascade")
	_, err := p.InvokeAgentTool(context.Background(), agentToolReq("add_tag", map[string]any{"tag_id": id, "task_id": "task-other"}))
	require.NoError(t, err)
	_, err = p.InvokeAgentTool(context.Background(), agentToolReq("add_tag", map[string]any{"tag_id": id}))
	require.NoError(t, err)
	require.Len(t, storedTagDoc(t, host, "ws-1").Tasks, 2)

	_, err = p.InvokeAgentTool(context.Background(), agentToolReq("delete_tag", map[string]any{"tag_id": id}))
	require.NoError(t, err)
	doc := storedTagDoc(t, host, "ws-1")
	require.Empty(t, doc.Tags)
	require.Empty(t, doc.Tasks)
}

// Workspace scoping is unchanged: a task_id only ever addresses a key inside
// the caller's own workspace document, never another workspace's.
func TestTargetedTagWritesStayInsideCallerWorkspace(t *testing.T) {
	p, host := newAgentTagTestPlugin()
	id := agentCreate(t, p, "Scoped")

	other := agentToolReq("add_tag", map[string]any{"tag_id": id, "task_id": "task-other"})
	other.Context.WorkspaceID = "ws-1"
	_, err := p.InvokeAgentTool(context.Background(), other)
	require.NoError(t, err)

	_, found, err := host.GetState(context.Background(), "workspace", "ws-2", tagStateKey)
	require.NoError(t, err)
	require.False(t, found, "no other workspace document may be created")
	require.Contains(t, storedTagDoc(t, host, "ws-1").Tasks, "task-other")
}

// Regression (QA): after 210 invented task-id attempts against the 200-task
// cap, neither a real human-applied task nor a real agent-applied task may be
// evicted. New agent-created keys fill the remaining slots, then fail closed
// without mutating the workspace document.
func TestQA_CapPressureFromOrphans(t *testing.T) {
	p, host := newAgentTagTestPlugin()
	id := agentCreate(t, p, "Spam")

	doc := storedTagDoc(t, host, "ws-1")
	doc.Tasks["real-human-task"] = []taskTag{{TagID: id, Human: true, UpdatedAt: "2000-01-01T00:00:00Z"}}
	doc.Tasks["real-agent-task"] = []taskTag{{TagID: id, Agent: true, UpdatedAt: "2000-01-01T00:00:00Z"}}
	raw, err := encodeTagDoc(doc)
	require.NoError(t, err)
	require.NoError(t, host.SetState(context.Background(), "workspace", "ws-1", tagStateKey, raw))

	admitted, rejected := 0, 0
	var fullBeforeRejections tagDoc
	for i := 0; i < tagTaskCap+10; i++ {
		if i == tagTaskCap-2 {
			fullBeforeRejections = storedTagDoc(t, host, "ws-1")
		}
		res, err := p.InvokeAgentTool(context.Background(), agentToolReq("add_tag", map[string]any{
			"tag_id":  id,
			"task_id": fmt.Sprintf("ghost-%04d", i),
		}))
		require.NoError(t, err)
		if res.IsError {
			rejected++
			require.Equal(t, fmt.Sprintf("workspace tag task capacity is %d; remove a task tag before targeting a new task", tagTaskCap), res.Text)
			continue
		}
		admitted++
	}

	require.Equal(t, tagTaskCap-2, admitted)
	require.Equal(t, 12, rejected)
	after := storedTagDoc(t, host, "ws-1")
	require.Len(t, after.Tasks, tagTaskCap)
	require.Equal(t, fullBeforeRejections, after, "rejected attempts must not mutate the stored document")
	require.True(t, after.Tasks["real-human-task"][0].Human)
	require.True(t, after.Tasks["real-agent-task"][0].Agent)
	for i := tagTaskCap - 2; i < tagTaskCap+10; i++ {
		require.NotContains(t, after.Tasks, fmt.Sprintf("ghost-%04d", i), "rejected target must not be persisted")
	}
}

func TestAgentCanUpdateExistingTasksAtCapAndReuseFreedSlot(t *testing.T) {
	p, host := newAgentTagTestPlugin()
	id := agentCreate(t, p, "Capacity")

	doc := storedTagDoc(t, host, "ws-1")
	for i := 0; i < tagTaskCap-2; i++ {
		doc.Tasks[fmt.Sprintf("peer-%03d", i)] = []taskTag{{TagID: id, Agent: true, UpdatedAt: "2020-01-01T00:00:00Z"}}
	}
	doc.Tasks["task-1"] = []taskTag{{TagID: id, Agent: true, Note: "old self", UpdatedAt: "2020-01-01T00:00:00Z"}}
	doc.Tasks["task-other"] = []taskTag{{TagID: id, Agent: true, Note: "old target", UpdatedAt: "2020-01-01T00:00:00Z"}}
	raw, err := encodeTagDoc(doc)
	require.NoError(t, err)
	require.NoError(t, host.SetState(context.Background(), "workspace", "ws-1", tagStateKey, raw))

	self, err := p.InvokeAgentTool(context.Background(), agentToolReq("add_tag", map[string]any{"tag_id": id, "note": "new self"}))
	require.NoError(t, err)
	require.False(t, self.IsError, self.Text)
	targeted, err := p.InvokeAgentTool(context.Background(), agentToolReq("add_tag", map[string]any{"tag_id": id, "task_id": "task-other", "note": "new target"}))
	require.NoError(t, err)
	require.False(t, targeted.IsError, targeted.Text)
	afterUpdates := storedTagDoc(t, host, "ws-1")
	require.Len(t, afterUpdates.Tasks, tagTaskCap)
	require.Equal(t, "new self", afterUpdates.Tasks["task-1"][0].Note)
	require.Equal(t, "new target", afterUpdates.Tasks["task-other"][0].Note)
	require.Contains(t, afterUpdates.Tasks, "peer-000", "existing-key updates must not evict a peer")

	removed, err := p.InvokeAgentTool(context.Background(), agentToolReq("remove_tag", map[string]any{"tag_id": id, "task_id": "task-other"}))
	require.NoError(t, err)
	require.False(t, removed.IsError, removed.Text)
	require.Len(t, storedTagDoc(t, host, "ws-1").Tasks, tagTaskCap-1)

	added, err := p.InvokeAgentTool(context.Background(), agentToolReq("add_tag", map[string]any{"tag_id": id, "task_id": "task-new"}))
	require.NoError(t, err)
	require.False(t, added.IsError, added.Text)
	afterReuse := storedTagDoc(t, host, "ws-1")
	require.Len(t, afterReuse.Tasks, tagTaskCap)
	require.Contains(t, afterReuse.Tasks, "task-new")
}

// Generic over-cap cleanup still discards an uncurated task before any task a
// human has curated. Agent add_tag no longer reaches this cleanup when a new
// target arrives at capacity; human actions and decoded state still can.
func TestCapEvictsUncuratedEntriesBeforeHumanAppliedOnes(t *testing.T) {
	p := &tagsPlugin{}
	doc := newTagDoc()
	for i := 0; i < tagTaskCap; i++ {
		doc.Tasks[fmt.Sprintf("real-%03d", i)] = []taskTag{{TagID: "human", Human: true, UpdatedAt: "2020-01-01T00:00:00Z"}}
	}
	doc.Tasks["uncurated"] = []taskTag{{TagID: "agent", Agent: true, UpdatedAt: "2030-01-01T00:00:00Z"}}
	p.capTagTasks(&doc)

	require.Len(t, doc.Tasks, tagTaskCap)
	require.NotContains(t, doc.Tasks, "uncurated", "uncurated entry is evicted even though it is newest")
	for i := 0; i < tagTaskCap; i++ {
		require.Contains(t, doc.Tasks, fmt.Sprintf("real-%03d", i), "no human-applied entry may be displaced")
	}
}

// Eviction must not depend on Go's randomized map iteration order: identical
// input must always drop the same task, or a data-loss path becomes a coin flip.
func TestCapEvictionIsDeterministicOnTiedTimestamps(t *testing.T) {
	victims := map[string]int{}
	for run := 0; run < 8; run++ {
		p := &tagsPlugin{}
		doc := newTagDoc()
		for i := 0; i <= tagTaskCap; i++ {
			doc.Tasks[fmt.Sprintf("tied-%03d", i)] = []taskTag{{TagID: "agent", Agent: true, UpdatedAt: "2026-01-01T00:00:00Z"}}
		}
		p.capTagTasks(&doc)
		require.Len(t, doc.Tasks, tagTaskCap)
		for i := 0; i <= tagTaskCap; i++ {
			if key := fmt.Sprintf("tied-%03d", i); !containsKey(doc.Tasks, key) {
				victims[key]++
			}
		}
	}
	require.Len(t, victims, 1, "the same task must be evicted every run, got %v", victims)
	require.Equal(t, 8, victims["tied-000"], "ties break on task id, lowest first")
}

func containsKey(m map[string][]taskTag, key string) bool {
	_, ok := m[key]
	return ok
}

// A task_id is an opaque map key, so hostile-looking values must be stored
// verbatim rather than interpreted, and must never produce a stray key. Padding
// is trimmed so an id copied from the board with surrounding whitespace still
// lands on the intended card.
func TestTargetTaskIDHandlesAdversarialValues(t *testing.T) {
	cases := []struct {
		label  string
		taskID any
		target string
	}{
		{"omitted", nil, "task-1"},
		{"explicit own task", "task-1", "task-1"},
		{"padded id is trimmed", "  task-2  ", "task-2"},
		{"unicode", "tâche-🚀-2", "tâche-🚀-2"},
		{"very long", strings.Repeat("x", 4096), strings.Repeat("x", 4096)},
		{"traversal shape is just a key", "../../other-ws/task", "../../other-ws/task"},
		{"json-ish is just a key", `{"a":1}`, `{"a":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			p, host := newAgentTagTestPlugin()
			id := agentCreate(t, p, "Probe")
			args := map[string]any{"tag_id": id}
			if tc.taskID != nil {
				args["task_id"] = tc.taskID
			}
			res, err := p.InvokeAgentTool(context.Background(), agentToolReq("add_tag", args))
			require.NoError(t, err)
			require.False(t, res.IsError, res.Text)
			doc := storedTagDoc(t, host, "ws-1")
			require.Contains(t, doc.Tasks, tc.target)
			require.Len(t, doc.Tasks, 1, "exactly one task key written")
			require.NotContains(t, doc.Tasks, "", "never an empty task key")
		})
	}
}

// The manifest types task_id as a string and the host validates against it, so
// a non-string cannot arrive in practice. Should that ever change, the resolver
// must fall back to the caller's own task rather than panic or mis-target.
func TestTargetTaskIDIgnoresNonStringArgument(t *testing.T) {
	for _, bad := range []any{42, 3.14, true, nil, []any{"a"}, map[string]any{"k": "v"}} {
		p, host := newAgentTagTestPlugin()
		id := agentCreate(t, p, "Probe")
		res, err := p.InvokeAgentTool(context.Background(), agentToolReq("add_tag", map[string]any{"tag_id": id, "task_id": bad}))
		require.NoError(t, err)
		require.False(t, res.IsError, res.Text)
		doc := storedTagDoc(t, host, "ws-1")
		require.Contains(t, doc.Tasks, "task-1", "non-string %T must fall back to the caller's task", bad)
		require.Len(t, doc.Tasks, 1)
	}
}

// Decision D4: validating the resolved target means an explicit task_id stands
// on its own when the invocation carries no task of its own, while omitting it
// still produces today's error. Workspace remains mandatory either way.
func TestResolvedTargetSatisfiesContextValidation(t *testing.T) {
	p, host := newAgentTagTestPlugin()
	id := agentCreate(t, p, "Probe")

	withTarget := agentToolReq("add_tag", map[string]any{"tag_id": id, "task_id": "target"})
	withTarget.Context.TaskID = ""
	res, err := p.InvokeAgentTool(context.Background(), withTarget)
	require.NoError(t, err)
	require.False(t, res.IsError, res.Text)
	require.Contains(t, storedTagDoc(t, host, "ws-1").Tasks, "target")

	bare := agentToolReq("add_tag", map[string]any{"tag_id": id})
	bare.Context.TaskID = ""
	res, err = p.InvokeAgentTool(context.Background(), bare)
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Equal(t, "task_id is required", res.Text)

	noWorkspace := agentToolReq("add_tag", map[string]any{"tag_id": id, "task_id": "target"})
	noWorkspace.Context.WorkspaceID = ""
	res, err = p.InvokeAgentTool(context.Background(), noWorkspace)
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Equal(t, "workspace_id is required", res.Text)
}

// Retrying a targeted call must update the single entry rather than accumulate
// duplicates, and notes are truncated on a targeted task exactly as on the
// caller's own. Repeated removes stay safe.
func TestTargetedApplicationIsIdempotentAndTruncatesNotes(t *testing.T) {
	p, host := newAgentTagTestPlugin()
	id := agentCreate(t, p, "Probe")
	for i := 0; i < 5; i++ {
		res, err := p.InvokeAgentTool(context.Background(), agentToolReq("add_tag", map[string]any{"tag_id": id, "task_id": "target", "note": fmt.Sprintf("try %d", i)}))
		require.NoError(t, err)
		require.False(t, res.IsError, res.Text)
	}
	entries := storedTagDoc(t, host, "ws-1").Tasks["target"]
	require.Len(t, entries, 1, "replay updates in place")
	require.Equal(t, "try 4", entries[0].Note)

	_, err := p.InvokeAgentTool(context.Background(), agentToolReq("add_tag", map[string]any{"tag_id": id, "task_id": "target", "note": strings.Repeat("é", 500)}))
	require.NoError(t, err)
	require.Len(t, []rune(storedTagDoc(t, host, "ws-1").Tasks["target"][0].Note), maxTagNoteRunes)

	for i := 0; i < 3; i++ {
		res, err := p.InvokeAgentTool(context.Background(), agentToolReq("remove_tag", map[string]any{"tag_id": id, "task_id": "target"}))
		require.NoError(t, err)
		require.False(t, res.IsError, res.Text)
	}
	require.NotContains(t, storedTagDoc(t, host, "ws-1").Tasks, "target")
}

// Targeting does not weaken the mutation lock: concurrent calls aimed at
// distinct tasks must all land.
func TestConcurrentTargetedWritesAllLand(t *testing.T) {
	p, host := newAgentTagTestPlugin()
	id := agentCreate(t, p, "Probe")
	var wg sync.WaitGroup
	const n = 40
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := p.InvokeAgentTool(context.Background(), agentToolReq("add_tag", map[string]any{"tag_id": id, "task_id": fmt.Sprintf("t-%03d", i)}))
			require.NoError(t, err)
		}(i)
	}
	wg.Wait()
	require.Len(t, storedTagDoc(t, host, "ws-1").Tasks, n)
}

// -----------------------------------------------------------------------
// The derived tag color (shared contract with ui/bundle.js)
// -----------------------------------------------------------------------

type tagColorFixture struct {
	Palette []string `json:"palette"`
	Neutral string   `json:"neutral"`
	Trim    []string `json:"trim"`
	Names   []struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	} `json:"names"`
}

func loadTagColorFixture(t *testing.T) tagColorFixture {
	t.Helper()
	raw, err := os.ReadFile("../testdata/tag-colors.json")
	require.NoError(t, err)
	var fixture tagColorFixture
	require.NoError(t, json.Unmarshal(raw, &fixture))
	require.NotEmpty(t, fixture.Names)
	return fixture
}

func tagByName(t *testing.T, doc tagDoc, name string) sharedTag {
	t.Helper()
	for _, tag := range doc.Tags {
		if tag.Name == name {
			return tag
		}
	}
	t.Fatalf("tag %q not found in catalog", name)
	return sharedTag{}
}

// The color of a name is a cross-language contract, not an implementation
// detail: a tag created in the Tags box and the same name created by an agent
// through create_tag have to come out identical. These pairs are the same ones
// ui/bundle.test.js asserts against colorFromName, so changing one hash alone
// fails one suite or the other. The names cover every branch of the UTF-8
// encoder (1-, 2-, 3-, and 4-byte sequences), the 22-rune cap, a name that
// collides with another in this table (accepted and harmless), and the empty
// string.
func TestAutoTagColorMatchesSharedFixture(t *testing.T) {
	for _, want := range loadTagColorFixture(t).Names {
		require.Equal(t, want.Color, autoTagColor(want.Name), "autoTagColor(%q)", want.Name)
	}
}

// A palette reorder on one side alone would silently give the same name two
// different colors, so both sides are pinned to the fixture's palette as well:
// here to tagColorPalette, and in ui/bundle.test.js to PALETTE.
func TestTagColorPaletteMatchesSharedFixture(t *testing.T) {
	fixture := loadTagColorFixture(t)
	require.Equal(t, fixture.Palette, tagColorPalette[:])
	// The neutral default is duplicated in both halves (defaultTagColor here,
	// DEFAULT_COLOR in ui/bundle.js) and decides what an auto_color-off tag looks
	// like -- and what any tag looks like on a host that predates plugin actions
	// -- so it is pinned to the fixture instead of drifting between the two.
	require.Equal(t, fixture.Neutral, defaultTagColor)
}

// Legacy v1 names were only trimmed and length-capped, so the migration must
// title them without destroying characters: capitalizing the first *byte* of a
// multi-byte rune produced U+FFFD plus fragments, and the next write persisted
// them. (v1 tags arrive as slugs, so "-" becomes a space and each word is titled.)
func TestTitleFromSlugKeepsMultiByteNames(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"urgent", "Urgent"},
		{"waiting-on-design", "Waiting On Design"},
		{"über-crash", "Über Crash"},
		{"🚀-launch", "🚀 Launch"},
		{"界-blocked", "界 Blocked"},
		{"  spaced  ", "Spaced"},
		{"", ""},
	} {
		require.Equal(t, tc.want, titleFromSlug(tc.in), "%q", tc.in)
		require.NotContains(t, titleFromSlug(tc.in), "\uFFFD", "%q", tc.in)
	}
}

// A stored name that predates the shared trim rule -- 0.14 and earlier trimmed
// with strings.TrimSpace, which keeps U+FEFF, and the migration copies a legacy
// tag's own spelling -- must still block a duplicate once normalized, or the
// catalog ends up with two chips whose names are the same after normalization.
func TestHasTagNameNormalizesTheStoredName(t *testing.T) {
	stored := []sharedTag{{ID: "legacy", Name: "bug\uFEFF", Color: defaultTagColor}}
	require.True(t, hasTagName(stored, "bug", ""), "a BOM-suffixed stored name blocks its normalized duplicate")
	require.True(t, hasTagName(stored, "BUG", ""), "and the case-insensitive rule still applies")
	require.False(t, hasTagName(stored, "bug", "legacy"), "a tag never clashes with itself")

	// The candidate is normalized by its caller, so a padded candidate is the
	// caller's business; the stored side is what this pins.
	padded := []sharedTag{{ID: "t1", Name: "  docs  ", Color: "#123456"}}
	require.True(t, hasTagName(padded, "docs", ""))
}

// The edge-trim set is the other half of the cross-language contract: a
// character only one side strips is stored under one spelling and looked up
// under the other (see stripFromEdges). Both implementations enumerate the set
// explicitly, so this sweeps every code point and compares what this side
// actually strips against the fixture -- in both directions, so a character
// added to one list and not the other fails here rather than in production.
func TestEdgeTrimMatchesSharedFixture(t *testing.T) {
	want := map[rune]bool{}
	for _, hex := range loadTagColorFixture(t).Trim {
		value, err := strconv.ParseInt(hex, 16, 32)
		require.NoError(t, err, hex)
		want[rune(value)] = true
	}
	require.NotEmpty(t, want)

	got := map[rune]bool{}
	for r := rune(0); r <= 0x10FFFF; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			// A Go string cannot hold a lone surrogate (encoding one yields
			// U+FFFD, which is not trimmed); the JS sweep skips the same range.
			continue
		}
		if stripFromEdges(r) {
			got[r] = true
		}
	}
	require.Equal(t, want, got)
}

// An unpaired surrogate cannot even be spelled in Go source, but it can arrive
// on the wire: the JSON decoder replaces it with U+FFFD before normalizeTagName
// runs, so that -- not the raw surrogate -- is the name the backend stores. The
// UI folds the same way (foldLoneSurrogates) precisely so its post-create lookup
// searches for the name that was written.
func TestUnpairedSurrogateArrivesFolded(t *testing.T) {
	var args struct {
		Name string `json:"name"`
	}
	require.NoError(t, json.Unmarshal([]byte(`{"name":"a\ud800b"}`), &args))
	name, err := normalizeTagName(args.Name)
	require.NoError(t, err)
	require.Equal(t, "a\ufffdb", name)
}

// hasTagName is the authoritative duplicate rule, and it is stricter than the
// UI's lowercased approximation (findTagByName in ui/bundle.js): for the
// characters whose Unicode simple case folding is not their lowercase form, the
// server refuses a name the board's local check would have allowed. Pinned here
// so the asymmetry stays deliberate and documented from both ends -- the UI's
// half is the test named "findTagByName approximates the backend's case
// folding", and a refused create surfaces the server's own message.
func TestHasTagNameUsesSimpleCaseFolding(t *testing.T) {
	tags := []sharedTag{{ID: "t1", Name: "Σ", Color: "#ef4444"}}
	for _, tc := range []struct {
		name string
		want bool
	}{
		// Identical, an ordinary case pair, and final sigma -- which folds to
		// sigma in Go but does not lowercase to it in JavaScript.
		{"Σ", true},
		{"σ", true},
		{"ς", true},
		// Long s and the Kelvin sign are not the stored sigma at all.
		{"ſ", false},
		{"s", false},
		{"\u212a", false},
	} {
		require.Equal(t, tc.want, hasTagName(tags, tc.name, ""), "%q", tc.name)
	}

	// The long-s pair, with the long s stored: the mirror of what the UI cannot
	// predict, since `"ſ".toLowerCase()` stays "ſ" while EqualFold unifies it.
	require.True(t, hasTagName([]sharedTag{{ID: "t2", Name: "ſ"}}, "s", ""))
	require.False(t, hasTagName([]sharedTag{{ID: "t3", Name: "İ"}}, "i", ""), "U+0130 has no simple folding to i")
}

// Colors are trimmed with that same set, so a stray BOM behaves identically on
// both sides: accepted in front of a hex value, and "not supplied" when it is
// all there is.
func TestColorTrimmingMatchesTheUINormalizer(t *testing.T) {
	color, err := normalizeTagColor("\ufeff#AbC")
	require.NoError(t, err)
	require.Equal(t, "#aabbcc", color)

	_, err = normalizeTagColor("\ufeff")
	require.ErrorContains(t, err, "3- or 6-digit hex")

	p, host := newAgentTagTestPlugin()
	result, err := p.InvokeAgentTool(context.Background(), agentToolReq("create_tag", map[string]any{"name": "docs", "color": "\ufeff"}))
	require.NoError(t, err)
	require.False(t, result.IsError, result.Text)
	require.Equal(t, autoTagColor("docs"), tagByName(t, storedTagDoc(t, host, "ws-1"), "docs").Color)
}

// The backend has to strip exactly what the UI strips before it hashes or
// stores a name: the UI looks a created tag up by its *own* trimmed name, and
// the "same name, same color" rule assumes both sides agree on what the name
// is. JavaScript's trim() and Go's strings.TrimSpace differ by exactly two
// characters, and both directions are covered here -- U+FEFF (a BOM pasted from
// a spreadsheet) is JS whitespace but not Go's, U+0085 (NEL) is Go's but not
// JS's. Mirrored by the normalizeName case in ui/bundle.test.js.
func TestNormalizeTagNameMatchesTheUINormalizer(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"leading BOM", "\ufeffbug", "bug"},
		{"trailing BOM", "bug\ufeff", "bug"},
		{"NEL is not JS whitespace", "bug\u0085", "bug\u0085"},
		{"NBSP and ideographic space", "\u00a0bug\u3000", "bug"},
		{"ordinary whitespace", "\t bug \n", "bug"},
		{"unicode space separators", "\u2000urgent\u200a", "urgent"},
	}
	for _, tc := range cases {
		got, err := normalizeTagName(tc.input)
		require.NoError(t, err, tc.name)
		require.Equal(t, tc.want, got, tc.name)
	}

	// Only trimmable characters still means no name at all.
	_, err := normalizeTagName("\ufeff\u3000\n")
	require.Error(t, err)
}

// Creation without a color must derive one, on both entry points an agent or
// a person uses. An explicit color must still win, and an empty one must not
// silently become the derived value on the update path.
//
// These wiring tests (this one, TestAutoColorSettingControlsDerivedColor, and
// TestUpdateRejectsEmptyColor) assert through autoTagColor, so they pin the
// wiring, the setting's effect, and the neutral-gray fallback -- not the hash
// or the palette themselves: a mutation to either leaves them green and is
// caught only by the fixture tests above, which is the intended split.
func TestCreateWithoutColorDerivesNameColor(t *testing.T) {
	p, host := newAgentTagTestPlugin()

	result, err := p.InvokeAgentTool(context.Background(), agentToolReq("create_tag", map[string]any{"name": "waiting on design"}))
	require.NoError(t, err)
	require.False(t, result.IsError, result.Text)
	require.Equal(t, autoTagColor("waiting on design"), tagByName(t, storedTagDoc(t, host, "ws-1"), "waiting on design").Color)

	result, err = p.InvokeAgentTool(context.Background(), agentToolReq("create_tag", map[string]any{"name": "agent empty", "color": ""}))
	require.NoError(t, err)
	require.False(t, result.IsError, result.Text)
	require.Equal(t, autoTagColor("agent empty"), tagByName(t, storedTagDoc(t, host, "ws-1"), "agent empty").Color)

	_, err = p.HandleAction(context.Background(), actionReq("tag-create", []byte(`{"name":"bug"}`)))
	require.NoError(t, err)
	require.Equal(t, autoTagColor("bug"), tagByName(t, storedTagDoc(t, host, "ws-1"), "bug").Color)

	_, err = p.HandleAction(context.Background(), actionReq("tag-create", []byte(`{"name":"urgent","color":"#123456"}`)))
	require.NoError(t, err)
	require.Equal(t, "#123456", tagByName(t, storedTagDoc(t, host, "ws-1"), "urgent").Color)

	result, err = p.InvokeAgentTool(context.Background(), agentToolReq("create_tag", map[string]any{"name": "bad color", "color": "not-a-color"}))
	require.NoError(t, err)
	require.True(t, result.IsError)
	require.Contains(t, result.Text, "3- or 6-digit hex")
}

// The auto_color setting gates the derivation and nothing else: an explicit
// color survives it (it is a default, not a policy over people's choices), it
// applies to both creation entry points, and anything but an explicit false --
// including a config read that fails -- resolves to the documented default of
// on, so a broken setting can never block tagging.
func TestAutoColorSettingControlsDerivedColor(t *testing.T) {
	cases := []struct {
		name      string
		config    map[string]any
		err       error
		autoColor bool
	}{
		{name: "setting on", config: map[string]any{tagColorSettingKey: true}, autoColor: true},
		{name: "setting off", config: map[string]any{tagColorSettingKey: false}, autoColor: false},
		{name: "setting never saved", config: map[string]any{}, autoColor: true},
		{name: "setting malformed", config: map[string]any{tagColorSettingKey: "false"}, autoColor: true},
		{name: "config read failed", err: errors.New("plugin config unavailable"), autoColor: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, host := newAgentTagTestPlugin()
			host.config = tc.config
			host.configErr = tc.err
			wantColor := func(name string) string {
				if tc.autoColor {
					return autoTagColor(name)
				}
				return defaultTagColor
			}

			_, err := p.HandleAction(context.Background(), actionReq("tag-create", []byte(`{"name":"bug"}`)))
			require.NoError(t, err)
			require.Equal(t, wantColor("bug"), tagByName(t, storedTagDoc(t, host, "ws-1"), "bug").Color)

			result, err := p.InvokeAgentTool(context.Background(), agentToolReq("create_tag", map[string]any{"name": "urgent"}))
			require.NoError(t, err)
			require.False(t, result.IsError, result.Text)
			require.Equal(t, wantColor("urgent"), tagByName(t, storedTagDoc(t, host, "ws-1"), "urgent").Color)

			_, err = p.HandleAction(context.Background(), actionReq("tag-create", []byte(`{"name":"docs","color":"#123456"}`)))
			require.NoError(t, err)
			require.Equal(t, "#123456", tagByName(t, storedTagDoc(t, host, "ws-1"), "docs").Color)
		})
	}
}

// An explicitly empty color means "change the color" on the agent tier too, and
// both tiers answer it the same way: the board's tag-update refuses "" rather
// than reading it as absent. Creation keeps the opposite convention -- there an
// empty color means "derive one" -- which is documented on the tool.
func TestAgentUpdateRejectsAnExplicitEmptyColor(t *testing.T) {
	p, host := newAgentTagTestPlugin()
	result, err := p.InvokeAgentTool(context.Background(), agentToolReq("create_tag", map[string]any{"name": "probe"}))
	require.NoError(t, err)
	require.False(t, result.IsError, result.Text)
	id := tagByName(t, storedTagDoc(t, host, "ws-1"), "probe").ID
	before := tagByName(t, storedTagDoc(t, host, "ws-1"), "probe")

	result, err = p.InvokeAgentTool(context.Background(), agentToolReq("update_tag", map[string]any{"tag_id": id, "name": "probe renamed", "color": ""}))
	require.NoError(t, err)
	require.True(t, result.IsError)
	require.Contains(t, result.Text, "3- or 6-digit hex")
	require.Equal(t, before.Name, tagByName(t, storedTagDoc(t, host, "ws-1"), "probe").Name, "the rename did not happen")

	// A whitespace-only color is the same request, and an omitted one still means
	// "leave the color alone".
	result, err = p.InvokeAgentTool(context.Background(), agentToolReq("update_tag", map[string]any{"tag_id": id, "color": "   "}))
	require.NoError(t, err)
	require.True(t, result.IsError)
	require.Contains(t, result.Text, "3- or 6-digit hex")

	result, err = p.InvokeAgentTool(context.Background(), agentToolReq("update_tag", map[string]any{"tag_id": id, "name": "probe renamed"}))
	require.NoError(t, err)
	require.False(t, result.IsError, result.Text)
	require.Equal(t, before.Color, tagByName(t, storedTagDoc(t, host, "ws-1"), "probe renamed").Color)
}

// A supplied-but-blank name is refused like a supplied-but-blank color, and like
// the board: the request is refused whole rather than reported as a success that
// changed only the color, and a blank name alone does not claim a color was
// missing.
func TestAgentUpdateRejectsASuppliedBlankName(t *testing.T) {
	p, host := newAgentTagTestPlugin()
	result, err := p.InvokeAgentTool(context.Background(), agentToolReq("create_tag", map[string]any{"name": "probe"}))
	require.NoError(t, err)
	require.False(t, result.IsError, result.Text)
	before := tagByName(t, storedTagDoc(t, host, "ws-1"), "probe")

	for _, args := range []map[string]any{
		{"tag_id": before.ID, "name": ""},
		{"tag_id": before.ID, "name": "   "},
		{"tag_id": before.ID, "name": "", "color": "#123456"},
		{"tag_id": before.ID, "name": "   ", "color": "#123456"},
	} {
		result, err = p.InvokeAgentTool(context.Background(), agentToolReq("update_tag", args))
		require.NoError(t, err, "%v", args)
		require.True(t, result.IsError, "%v", args)
		require.Contains(t, result.Text, "tag name is required", "%v", args)
	}
	require.Equal(t, before, tagByName(t, storedTagDoc(t, host, "ws-1"), "probe"), "nothing changed")

	// Whichever field is omitted still means "leave it alone".
	result, err = p.InvokeAgentTool(context.Background(), agentToolReq("update_tag", map[string]any{"tag_id": before.ID, "name": "probe renamed"}))
	require.NoError(t, err)
	require.False(t, result.IsError, result.Text)
	after := tagByName(t, storedTagDoc(t, host, "ws-1"), "probe renamed")
	require.Equal(t, before.Color, after.Color)
	require.NotEqual(t, before.Name, after.Name)
}

// A duplicate name is a domain conflict, not an invocation failure. The host
// reports a Go error from a browser action as a generic 503 ("plugin action
// unavailable", the message going to the log instead), so the refusal has to
// come back as a status and body the host relays verbatim -- otherwise the board
// can only say "please try again", advice that can never work for the
// case-folding pairs its local check cannot predict (see
// TestHasTagNameUsesSimpleCaseFolding).
func TestDuplicateNameIsRefusedAsADomainConflict(t *testing.T) {
	p, host := newAgentTagTestPlugin()
	_, err := p.HandleAction(context.Background(), actionReq("tag-create", []byte(`{"name":"Σ"}`)))
	require.NoError(t, err)
	_, err = p.HandleAction(context.Background(), actionReq("tag-create", []byte(`{"name":"omicron"}`)))
	require.NoError(t, err)
	original := storedTagDoc(t, host, "ws-1")

	response, err := p.HandleAction(context.Background(), actionReq("tag-create", []byte(`{"name":"ς"}`)))
	require.NoError(t, err, "a refused duplicate is a response, not an invocation error")
	require.Equal(t, http.StatusConflict, response.Status)
	var body map[string]string
	require.NoError(t, json.Unmarshal(response.Body, &body))
	require.Equal(t, `a tag named "ς" already exists`, body["error"])
	require.Equal(t, "application/json", response.Headers["Content-Type"])
	require.Equal(t, original, storedTagDoc(t, host, "ws-1"), "the refusal writes nothing")

	// Renaming a *different* tag onto an existing name answers the same way (a tag
	// renamed to its own current name is not a conflict), and writes nothing either.
	id := tagByName(t, original, "omicron").ID
	response, err = p.HandleAction(context.Background(), actionReq("tag-update", []byte(`{"id":"`+id+`","name":"Σ"}`)))
	require.NoError(t, err)
	require.Equal(t, http.StatusConflict, response.Status)
	require.Equal(t, original, storedTagDoc(t, host, "ws-1"))

	// Agent tools keep the plain message: their result is text the agent reads,
	// with no transport in between to lose it.
	result, err := p.InvokeAgentTool(context.Background(), agentToolReq("create_tag", map[string]any{"name": "ς"}))
	require.NoError(t, err)
	require.True(t, result.IsError)
	require.Contains(t, result.Text, `a tag named "ς" already exists`)
}

// An update never derives: a stored color is replaced only by a validated
// one, so an explicit empty color is rejected rather than quietly reset, and
// an update that omits the color leaves the tag's existing one alone (even
// when that color came from the name rather than an explicit pick).
func TestUpdateRejectsEmptyColor(t *testing.T) {
	p, host := newAgentTagTestPlugin()
	_, err := p.HandleAction(context.Background(), actionReq("tag-create", []byte(`{"name":"bug","color":"#123456"}`)))
	require.NoError(t, err)
	id := tagByName(t, storedTagDoc(t, host, "ws-1"), "bug").ID

	_, err = p.HandleAction(context.Background(), actionReq("tag-update", []byte(`{"id":"`+id+`","color":""}`)))
	require.ErrorContains(t, err, "3- or 6-digit hex")
	require.Equal(t, "#123456", tagByName(t, storedTagDoc(t, host, "ws-1"), "bug").Color)

	result, err := p.InvokeAgentTool(context.Background(), agentToolReq("create_tag", map[string]any{"name": "probe"}))
	require.NoError(t, err)
	require.False(t, result.IsError, result.Text)
	probeID := tagByName(t, storedTagDoc(t, host, "ws-1"), "probe").ID

	result, err = p.InvokeAgentTool(context.Background(), agentToolReq("update_tag", map[string]any{"tag_id": probeID, "name": "probe renamed"}))
	require.NoError(t, err)
	require.False(t, result.IsError, result.Text)
	require.Equal(t, autoTagColor("probe"), tagByName(t, storedTagDoc(t, host, "ws-1"), "probe renamed").Color)
}
