package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

func jsonAction(value any) (*pluginsdk.PluginActionResponse, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return &pluginsdk.PluginActionResponse{Body: body, Headers: map[string]string{"Content-Type": "application/json"}}, nil
}

// actionError maps a mutation failure to the response the browser should see.
//
// A duplicate name is a domain conflict the person has to be able to tell apart
// from a transient failure -- the board's own duplicate check folds case
// differently from the backend, so it cannot predict every refusal -- and the
// host reports a Go error from a browser action only as a generic 503
// (internal/plugins/action_handlers.go logs the message instead of relaying
// it). Answering with a status and body the host passes through verbatim is
// therefore the only way that message reaches the UI, which is also why the
// status comes from the SDK's own category projection rather than a literal:
// "conflict" maps to 409 there, and the bundle treats only null/502/503/504 as
// retryable, so the refusal is never auto-retried.
//
// Every other failure keeps the previous behaviour (a transport-level error the
// host surfaces as 503), which the UI already reports as retryable.
func actionError(err error) (*pluginsdk.PluginActionResponse, error) {
	var duplicate duplicateTagNameError
	if !errors.As(err, &duplicate) {
		return nil, err
	}
	body, marshalErr := json.Marshal(map[string]string{"error": err.Error()})
	if marshalErr != nil {
		return nil, marshalErr
	}
	return &pluginsdk.PluginActionResponse{
		Status:  pluginsdk.ActionErrorHTTPStatus(pluginsdk.ActionErrorConflict),
		Body:    body,
		Headers: map[string]string{"Content-Type": "application/json"},
	}, nil
}

func requireActionWorkspace(req *pluginsdk.PluginActionRequest) error {
	if req.Context.WorkspaceID == "" {
		return fmt.Errorf("workspace_id is required")
	}
	return nil
}
func requireActionTask(req *pluginsdk.PluginActionRequest) error {
	if err := requireActionWorkspace(req); err != nil {
		return err
	}
	if req.Context.TaskID == "" {
		return fmt.Errorf("task_id is required")
	}
	return nil
}
func decodeAction(req *pluginsdk.PluginActionRequest, target any) error {
	if len(req.Body) == 0 {
		return fmt.Errorf("action body is required")
	}
	if err := json.Unmarshal(req.Body, target); err != nil {
		return fmt.Errorf("invalid action body: %w", err)
	}
	return nil
}

// workspaceView is the response of shared-tags, tag-create, tag-update and
// tag-delete. The host rejects an action response above its own byte cap
// (maxPluginActionResponseBytes, 1 MiB in kandev) with a 502, and the UI
// classifies a 502 as retryable -- so an over-cap payload stops that workspace's
// catalog from loading at all and retries forever, which is worse than a partial
// one. Nothing here bounds the size: tagTaskCap caps task *keys* at 200, entries
// per task are uncapped, and each note may be 200 runes of up to four bytes, so
// an extreme agent-written board can cross the cap. Deliberately left as a known
// risk rather than "fixed" here: shrinking the payload means choosing what to
// drop (notes, task keys, or a paginated read), which changes the contract for
// every surface instead of hardening one.
func workspaceView(doc tagDoc) map[string]any {
	tasks := map[string]any{}
	for taskID, entries := range doc.Tasks {
		if view := taskTagView(doc, entries); len(view) > 0 {
			tasks[taskID] = view
		}
	}
	return map[string]any{"tags": catalogView(doc), "tasks": tasks}
}

func (p *tagsPlugin) HandleAction(ctx context.Context, req *pluginsdk.PluginActionRequest) (*pluginsdk.PluginActionResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("action request is missing")
	}
	switch req.ActionKey {
	case "shared-tags":
		if err := requireActionWorkspace(req); err != nil {
			return nil, err
		}
		doc, err := p.readTagDoc(ctx, req.Context.WorkspaceID)
		if err != nil {
			return nil, err
		}
		return jsonAction(workspaceView(doc))
	case "tag-create":
		if err := requireActionWorkspace(req); err != nil {
			return nil, err
		}
		var body struct {
			Name  string `json:"name"`
			Color string `json:"color"`
		}
		if err := decodeAction(req, &body); err != nil {
			return nil, err
		}
		name, err := normalizeTagName(body.Name)
		if err != nil {
			return nil, err
		}
		color, err := p.newTagColor(ctx, name, body.Color)
		if err != nil {
			return nil, err
		}
		doc, err := p.mutateTagDoc(ctx, req.Context.WorkspaceID, func(doc *tagDoc) error {
			if hasTagName(doc.Tags, name, "") {
				return duplicateTagNameError{name: name}
			}
			id, err := newTagID()
			if err != nil {
				return err
			}
			timestamp := now()
			doc.Tags = append(doc.Tags, sharedTag{ID: id, Name: name, Color: color, Owner: ownerHuman, CreatedAt: timestamp, UpdatedAt: timestamp})
			return nil
		})
		if err != nil {
			return actionError(err)
		}
		return jsonAction(workspaceView(doc))
	case "tag-update":
		if err := requireActionWorkspace(req); err != nil {
			return nil, err
		}
		var body struct {
			ID    string  `json:"id"`
			Name  *string `json:"name"`
			Color *string `json:"color"`
		}
		if err := decodeAction(req, &body); err != nil {
			return nil, err
		}
		if body.ID == "" {
			return nil, fmt.Errorf("id is required")
		}
		if body.Name == nil && body.Color == nil {
			return nil, fmt.Errorf("name or color is required")
		}
		doc, err := p.mutateTagDoc(ctx, req.Context.WorkspaceID, func(doc *tagDoc) error {
			i := findTagIndex(doc.Tags, body.ID)
			if i < 0 {
				return fmt.Errorf("tag %q does not exist", body.ID)
			}
			tag := &doc.Tags[i]
			if body.Name != nil {
				name, err := normalizeTagName(*body.Name)
				if err != nil {
					return err
				}
				if hasTagName(doc.Tags, name, body.ID) {
					return duplicateTagNameError{name: name}
				}
				tag.Name = name
			}
			if body.Color != nil {
				color, err := normalizeTagColor(*body.Color)
				if err != nil {
					return err
				}
				tag.Color = color
			}
			tag.UpdatedAt = now()
			return nil
		})
		if err != nil {
			return actionError(err)
		}
		return jsonAction(workspaceView(doc))
	case "tag-delete":
		if err := requireActionWorkspace(req); err != nil {
			return nil, err
		}
		var body struct {
			ID string `json:"id"`
		}
		if err := decodeAction(req, &body); err != nil {
			return nil, err
		}
		if body.ID == "" {
			return nil, fmt.Errorf("id is required")
		}
		doc, err := p.mutateTagDoc(ctx, req.Context.WorkspaceID, func(doc *tagDoc) error {
			if findTagIndex(doc.Tags, body.ID) < 0 {
				return fmt.Errorf("tag %q does not exist", body.ID)
			}
			deleteTag(doc, body.ID)
			return nil
		})
		if err != nil {
			return nil, err
		}
		return jsonAction(workspaceView(doc))
	case "task-tag-add":
		if err := requireActionTask(req); err != nil {
			return nil, err
		}
		var body struct {
			TagID string `json:"tagId"`
		}
		if err := decodeAction(req, &body); err != nil {
			return nil, err
		}
		if body.TagID == "" {
			return nil, fmt.Errorf("tagId is required")
		}
		doc, err := p.mutateTagDoc(ctx, req.Context.WorkspaceID, func(doc *tagDoc) error {
			if findTag(doc.Tags, body.TagID) == nil {
				return fmt.Errorf("tag %q does not exist", body.TagID)
			}
			entries := doc.Tasks[req.Context.TaskID]
			i := findTaskTagIndex(entries, body.TagID)
			if i < 0 {
				entries = append(entries, taskTag{TagID: body.TagID, Human: true, UpdatedAt: now()})
			} else {
				entries[i].Human = true
				entries[i].UpdatedAt = now()
			}
			doc.Tasks[req.Context.TaskID] = entries
			return nil
		})
		if err != nil {
			return nil, err
		}
		return jsonAction(map[string]any{"tags": taskTagView(doc, doc.Tasks[req.Context.TaskID])})
	case "task-tag-remove":
		if err := requireActionTask(req); err != nil {
			return nil, err
		}
		var body struct {
			TagID string `json:"tagId"`
		}
		if err := decodeAction(req, &body); err != nil {
			return nil, err
		}
		if body.TagID == "" {
			return nil, fmt.Errorf("tagId is required")
		}
		doc, err := p.mutateTagDoc(ctx, req.Context.WorkspaceID, func(doc *tagDoc) error {
			entries := doc.Tasks[req.Context.TaskID]
			i := findTaskTagIndex(entries, body.TagID)
			if i < 0 {
				return nil
			}
			entries = append(entries[:i], entries[i+1:]...)
			if len(entries) == 0 {
				delete(doc.Tasks, req.Context.TaskID)
			} else {
				doc.Tasks[req.Context.TaskID] = entries
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		return jsonAction(map[string]any{"tags": taskTagView(doc, doc.Tasks[req.Context.TaskID])})
	default:
		return nil, fmt.Errorf("unknown action: %s", req.ActionKey)
	}
}
