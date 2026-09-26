package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

const (
	// tagStateKey deliberately keeps the original key. Version 2 turns the
	// former agent-only status document into the shared workspace catalog.
	tagStateKey     = "agent-tags"
	tagTaskCap      = 200
	maxTagNameRunes = 22
	maxTagNoteRunes = 200
	// defaultTagColor is the neutral gray for a tag that has no color of its
	// own: the pre-catalog legacy entries migrateLegacyTagDoc rebuilds; a
	// legacy v1 plain-string tag the catalog cannot resolve (as DEFAULT_COLOR in
	// ui/bundle.js); and a new tag while the auto_color setting is off.
	// Otherwise new tags get autoTagColor(name) or an explicit color.
	defaultTagColor = "#6b7280"
	// tagColorSettingKey is the manifest config_schema property that turns
	// name-derived colors on and off. Absent means "never set", which resolves
	// to on -- see tagColorAutoEnabled.
	tagColorSettingKey = "auto_color"
	ownerAgent         = "agent"
	ownerHuman         = "human"
)

var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{3}([0-9a-fA-F]{3})?$`)

// tagColorPalette is the plugin's color vocabulary and the source of every
// derived tag color. ui/bundle.js holds the same curated fourteen-color set as
// PALETTE -- the UI renders the picker's swatches from its copy -- so the two
// lists must stay identical. TestTagColorPaletteMatchesSharedFixture asserts
// this list against testdata/tag-colors.json, which ui/bundle.test.js asserts
// PALETTE against, so a reorder on either side alone fails its own suite.
var tagColorPalette = [14]string{"#b45309", "#c2410c", "#b91c1c", "#be185d", "#a21caf", "#6d28d9", "#4338ca", "#1d4ed8", "#0369a1", "#0e7490", "#0f766e", "#15803d", "#4d7c0f", "#475569"}

// autoTagColor derives a tag's default color from its name -- the same
// content-addressed scheme Proxmox tags and GitHub labels use, so a name looks
// the same wherever and by whomever it is created, with no stored randomness
// and no allocation. It is FNV-1a (32-bit) over the name's UTF-8 bytes,
// matched byte for byte by colorFromName in ui/bundle.js, reduced to an index
// into tagColorPalette.
//
// The name is hashed exactly as normalizeTagName stores it: trimmed, case
// preserved. Two names differing only in case cannot coexist in one catalog
// (hasTagName compares case-insensitively), so case sensitivity costs nothing
// and keeps the two implementations trivially in step.
//
// Collisions are expected, unremarkable, and cannot mislead: color is
// decoration, the chip always carries the name, and a person who wants the
// color to mean something can set it with the picker -- which is what makes
// this a default rather than a constraint.
func autoTagColor(name string) string {
	const offset32 = 2166136261
	const prime32 = 16777619
	hash := uint32(offset32)
	for i := range len(name) {
		hash ^= uint32(name[i])
		hash *= prime32
	}
	return tagColorPalette[hash%uint32(len(tagColorPalette))]
}

// newTagColor returns the color a *new* tag should store. An explicit hex from
// the caller always wins -- the setting never overrides a person's or an
// agent's choice. Without one, the name-derived default is stored while the
// auto_color setting is on, and the neutral gray while the operator has turned
// it off, so a color-less tag stays neutral until somebody picks a color in
// the Tags box. Creation is the only path where a color may be omitted, so
// this is the only resolver; an update that carries one validates it outright.
func (p *tagsPlugin) newTagColor(ctx context.Context, name, color string) (string, error) {
	if strings.TrimFunc(color, stripFromEdges) != "" {
		return normalizeTagColor(color)
	}
	if p.tagColorAutoEnabled(ctx) {
		return autoTagColor(name), nil
	}
	return defaultTagColor, nil
}

// tagColorAutoEnabled reads the operator's auto_color setting (Settings >
// Plugins > <plugin>, the form generated from the manifest's config_schema).
// Everything except an explicit false means on: the key is absent until someone
// opens that page, and deriving colors is the plugin's documented default. A
// malformed value or a failed config read resolves to that same default rather
// than erroring -- an unreadable setting must not stop a person or an agent
// from tagging a card.
//
// Read per creation rather than cached: kandev restarts a running plugin when
// its config changes, but creation is rare enough that a fresh read costs
// nothing and cannot go stale.
func (p *tagsPlugin) tagColorAutoEnabled(ctx context.Context) bool {
	host := p.Host()
	if host == nil {
		return true
	}
	config, err := host.GetConfig(ctx)
	if err != nil {
		return true
	}
	enabled, ok := config[tagColorSettingKey].(bool)
	return !ok || enabled
}

// sharedTag is workspace-visible. Owner is an origin rather than an identity:
// agent tools have no user identity and every authenticated human may manage
// the shared catalog.
type sharedTag struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Color     string `json:"color"`
	Owner     string `json:"owner"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// taskTag represents one visual chip. Keeping human and agent application
// separately means an agent cannot remove a human application of the same tag.
type taskTag struct {
	TagID     string `json:"tag_id"`
	Agent     bool   `json:"agent"`
	Human     bool   `json:"human"`
	Note      string `json:"note"`
	SessionID string `json:"session_id"`
	UpdatedAt string `json:"updated_at"`
}

type tagDoc struct {
	Version int                  `json:"version"`
	Tags    []sharedTag          `json:"tags"`
	Tasks   map[string][]taskTag `json:"tasks"`
}

// legacyTagDoc decodes the 0.7.0 static-status document so existing agent
// applications continue to render after the shared-catalog upgrade.
type legacyTagEntry struct {
	Tag       string `json:"tag"`
	Note      string `json:"note"`
	SessionID string `json:"session_id"`
	UpdatedAt string `json:"updated_at"`
}
type legacyTagDoc struct {
	Version int                         `json:"version"`
	Tasks   map[string][]legacyTagEntry `json:"tasks"`
}

func newTagDoc() tagDoc {
	return tagDoc{Version: 2, Tags: []sharedTag{}, Tasks: map[string][]taskTag{}}
}

func decodeTagDoc(raw map[string]any) (tagDoc, error) {
	if len(raw) == 0 {
		return newTagDoc(), nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return tagDoc{}, err
	}
	var probe struct {
		Tags json.RawMessage `json:"tags"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return tagDoc{}, err
	}
	if len(probe.Tags) == 0 || string(probe.Tags) == "null" {
		var legacy legacyTagDoc
		if err := json.Unmarshal(b, &legacy); err != nil {
			return tagDoc{}, err
		}
		return migrateLegacyTagDoc(legacy), nil
	}
	doc := newTagDoc()
	if err := json.Unmarshal(b, &doc); err != nil {
		return tagDoc{}, err
	}
	if doc.Tasks == nil {
		doc.Tasks = map[string][]taskTag{}
	}
	if doc.Tags == nil {
		doc.Tags = []sharedTag{}
	}
	doc.Version = 2
	return doc, nil
}

func migrateLegacyTagDoc(legacy legacyTagDoc) tagDoc {
	doc := newTagDoc()
	for taskID, entries := range legacy.Tasks {
		for _, entry := range entries {
			id := "agent-legacy-" + strings.TrimSpace(entry.Tag)
			if id == "agent-legacy-" {
				continue
			}
			// Legacy definitions keep the neutral gray rather than an
			// autoTagColor(name) derivation: they are not new tags, the gray
			// is what they have always rendered as, and a migration write
			// must not silently restyle anyone's existing board.
			if findTag(doc.Tags, id) == nil {
				doc.Tags = append(doc.Tags, sharedTag{ID: id, Name: titleFromSlug(entry.Tag), Color: defaultTagColor, Owner: ownerAgent, CreatedAt: entry.UpdatedAt, UpdatedAt: entry.UpdatedAt})
			}
			doc.Tasks[taskID] = append(doc.Tasks[taskID], taskTag{TagID: id, Agent: true, Note: truncateNote(entry.Note), SessionID: entry.SessionID, UpdatedAt: entry.UpdatedAt})
		}
	}
	return doc
}

// titleFromSlug titles a legacy v1 slug for the migrated catalog. It capitalizes
// the first *rune*, not the first byte: slicing one byte off a multi-byte rune
// yields a name that decodes as U+FFFD plus fragments and would be persisted on
// the next write. v1 itself could only hold its six fixed ASCII slugs (a
// vocabulary re-validated on every write), so this is defence in depth against a
// document edited or written out of band, not a repair of live v1 data.
func titleFromSlug(v string) string {
	words := strings.Fields(strings.ReplaceAll(strings.TrimSpace(v), "-", " "))
	for i := range words {
		runes := []rune(words[i])
		words[i] = string(unicode.ToUpper(runes[0])) + string(runes[1:])
	}
	return strings.Join(words, " ")
}

func encodeTagDoc(doc tagDoc) (map[string]any, error) {
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	err = json.Unmarshal(b, &raw)
	return raw, err
}

// stripFromEdges reports whether r is stripped from both ends of a
// user-supplied string -- a tag name or a color.
//
// The set is exactly JavaScript's String.prototype.trim -- ECMAScript's
// WhiteSpace plus its line terminators -- because that is what the UI applies to
// every name and color it sends or looks up. It is *not* Go's unicode.IsSpace:
// the two differ by exactly two characters, and both directions of that
// difference are user-visible. JS trims U+FEFF (the BOM a spreadsheet paste
// carries) where IsSpace does not, and IsSpace trims U+0085 (NEL) where JS does
// not. Left divergent for names, the create-and-apply flow's lookup (which
// compares the created tag's name against the *client's* trimmed name) misses,
// so the person sees "Could not create tag" while the tag sits in the catalog
// under the server's spelling -- and the same asymmetry would break the
// "same name, same color" rule this release documents.
//
// The list is written out rather than delegated to unicode.IsSpace so it cannot
// move under a Unicode revision, and testdata/tag-colors.json ("trim") holds
// the same code points as the shared contract:
// TestEdgeTrimMatchesSharedFixture sweeps every code point in both directions
// and fails if this set and the UI's EDGE_TRIM_RE ever disagree.
func stripFromEdges(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ',
		0x00a0, // NO-BREAK SPACE
		0x1680, // OGHAM SPACE MARK
		0x2028, // LINE SEPARATOR
		0x2029, // PARAGRAPH SEPARATOR
		0x202f, // NARROW NO-BREAK SPACE
		0x205f, // MEDIUM MATHEMATICAL SPACE
		0x3000, // IDEOGRAPHIC SPACE
		0xfeff: // ZERO WIDTH NO-BREAK SPACE (BOM)
		return true
	}
	// The remaining Unicode space separators (Zs): EN QUAD..HAIR SPACE.
	return r >= 0x2000 && r <= 0x200a
}

// normalizeTagName trims a supplied name with the shared edge set. maxTagNameRunes
// bounds it in runes, which is deliberately *looser* than the UI's
// MAX_TAG_LENGTH, which counts UTF-16 code units: a name of astral characters
// can be accepted here that the board's inputs refuse. The UI is the stricter
// side on purpose -- its cap is calibrated against the Create input's width, not
// against what the backend can store -- and a name the UI cannot type cannot
// reach the post-create lookup that requires the two to agree.
func normalizeTagName(name string) (string, error) {
	name = strings.TrimFunc(name, stripFromEdges)
	if name == "" {
		return "", fmt.Errorf("tag name is required")
	}
	if utf8.RuneCountInString(name) > maxTagNameRunes {
		return "", fmt.Errorf("tag name must be at most %d characters", maxTagNameRunes)
	}
	return name, nil
}

// normalizeTagColor validates a *supplied* color: a 3- or 6-digit hex value,
// expanded to six and lowercased. An empty color is an error here rather than
// the neutral default -- creation resolves it through newTagColor instead, and
// an update that omits the field never reaches this function at all. Trimming
// uses the same shared edge set as names, so a color that arrives with a stray
// BOM is accepted here exactly as the UI's normalizeColor accepts it.
func normalizeTagColor(color string) (string, error) {
	color = strings.TrimFunc(color, stripFromEdges)
	if !hexColor.MatchString(color) {
		return "", fmt.Errorf("color must be a 3- or 6-digit hex value")
	}
	color = strings.ToLower(color)
	if len(color) == 4 {
		return fmt.Sprintf("#%c%c%c%c%c%c", color[1], color[1], color[2], color[2], color[3], color[3]), nil
	}
	return color, nil
}

func truncateNote(note string) string {
	r := []rune(note)
	if len(r) > maxTagNoteRunes {
		r = r[:maxTagNoteRunes]
	}
	return string(r)
}

func findTag(tags []sharedTag, id string) *sharedTag {
	for i := range tags {
		if tags[i].ID == id {
			return &tags[i]
		}
	}
	return nil
}
func findTagIndex(tags []sharedTag, id string) int {
	for i := range tags {
		if tags[i].ID == id {
			return i
		}
	}
	return -1
}

// hasTagName is the authoritative duplicate rule. The stored name is normalized
// before the comparison: "every stored name is already normalized" is not a
// guarantee -- 0.14 and earlier trimmed with strings.TrimSpace, which keeps
// U+FEFF, and migrateLegacyTagDoc copies a legacy tag's own spelling -- so a
// stored "bug\uFEFF" would otherwise let a second tag whose name normalizes to
// the same "bug" through, giving two chips with one name. The candidate is
// already normalized by its caller.
func hasTagName(tags []sharedTag, name, exceptID string) bool {
	for _, tag := range tags {
		if tag.ID == exceptID {
			continue
		}
		if strings.EqualFold(strings.TrimFunc(tag.Name, stripFromEdges), name) {
			return true
		}
	}
	return false
}
func findTaskTagIndex(entries []taskTag, tagID string) int {
	for i := range entries {
		if entries[i].TagID == tagID {
			return i
		}
	}
	return -1
}
func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
func newTagID() (string, error) {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "tag-" + hex.EncodeToString(b), nil
}

func (p *tagsPlugin) readTagDoc(ctx context.Context, workspaceID string) (tagDoc, error) {
	host := p.Host()
	if host == nil {
		return tagDoc{}, fmt.Errorf("plugin host is unavailable")
	}
	raw, found, err := host.GetState(ctx, "workspace", workspaceID, tagStateKey)
	if err != nil {
		return tagDoc{}, err
	}
	if !found {
		return newTagDoc(), nil
	}
	return decodeTagDoc(raw)
}
func (p *tagsPlugin) mutateTagDoc(ctx context.Context, workspaceID string, mutate func(*tagDoc) error) (tagDoc, error) {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	doc, err := p.readTagDoc(ctx, workspaceID)
	if err != nil {
		return tagDoc{}, err
	}
	if err := mutate(&doc); err != nil {
		return tagDoc{}, err
	}
	p.capTagTasks(&doc)
	raw, err := encodeTagDoc(doc)
	if err != nil {
		return tagDoc{}, err
	}
	if err := p.Host().SetState(ctx, "workspace", workspaceID, tagStateKey, raw); err != nil {
		return tagDoc{}, err
	}
	return doc, nil
}

// capTagTasks is the general bound for mutations that can produce an over-cap
// document, including human actions and migrated state. Eviction prefers
// entries no human has curated. Within a class the oldest entry goes first,
// and the task id breaks ties so eviction is deterministic instead of following
// Go's randomized map iteration order. Agent add_tag applies the stricter
// ensureAgentTaskSlot admission rule before it can create an over-cap document.
func (p *tagsPlugin) capTagTasks(doc *tagDoc) {
	for len(doc.Tasks) > tagTaskCap {
		var victimID, victimOldest string
		victimHuman := false
		for id, entries := range doc.Tasks {
			human, oldest := false, ""
			for _, entry := range entries {
				if entry.Human {
					human = true
				}
				if oldest == "" || entry.UpdatedAt < oldest {
					oldest = entry.UpdatedAt
				}
			}
			if victimID == "" || evictBefore(human, oldest, id, victimHuman, victimOldest, victimID) {
				victimID, victimHuman, victimOldest = id, human, oldest
			}
		}
		delete(doc.Tasks, victimID)
	}
}

// ensureAgentTaskSlot prevents an agent-created task key from driving eviction
// against existing cards. Existing targets remain writable at capacity; a new
// target is admitted only while the workspace document has a free task slot.
func ensureAgentTaskSlot(doc tagDoc, taskID string) error {
	if _, exists := doc.Tasks[taskID]; exists || len(doc.Tasks) < tagTaskCap {
		return nil
	}
	return fmt.Errorf("workspace tag task capacity is %d; remove a task tag before targeting a new task", tagTaskCap)
}

// evictBefore reports whether the task entry described by (human, oldest, id)
// should be evicted ahead of the current candidate.
func evictBefore(human bool, oldest, id string, otherHuman bool, otherOldest, otherID string) bool {
	if human != otherHuman {
		return !human
	}
	if oldest != otherOldest {
		return oldest < otherOldest
	}
	return id < otherID
}

func requireToolContext(req *pluginsdk.AgentToolRequest) string {
	if req == nil {
		return "request is missing"
	}
	// Validating the resolved target rather than the raw context keeps the
	// existing error for a callerless invocation while letting an explicit
	// task_id stand on its own.
	if targetTaskID(req) == "" {
		return "task_id is required"
	}
	if req.Context.WorkspaceID == "" {
		return "workspace_id is required"
	}
	return ""
}

// targetTaskID resolves the task a tool acts on: the optional task_id argument
// when supplied, otherwise the calling agent's own task. This is the single
// fallback point -- no call site reads req.Context.TaskID directly.
//
// A supplied ID is not verified to name a real task; the plugin has no platform
// client to ask. Workspace scoping still contains it: doc.Tasks is a map inside
// the caller's own workspace document, so a target can only ever address a task
// in that workspace, and the deleteTag cascade still clears every key.
//
// An entry for a task that does not exist renders on no card and occupies one
// of the tagTaskCap slots. Unknown IDs remain accepted while capacity is
// available; ensureAgentTaskSlot rejects a new key at the cap so an invented ID
// cannot evict an existing card's agent- or human-applied tags.
func targetTaskID(req *pluginsdk.AgentToolRequest) string {
	if id := strings.TrimSpace(agentArgString(req, "task_id")); id != "" {
		return id
	}
	return req.Context.TaskID
}

// duplicateTagNameError marks a name collision as a domain conflict rather than
// an invocation failure. The host relays a Go error from a browser action as a
// generic 503 whose text is only logged (internal/plugins/action_handlers.go),
// so a refusal the person has to distinguish -- "this name already exists" --
// has to come back as a status the host passes through verbatim: see actionError
// in server/actions.go. Agent tools have no such relay problem and keep
// reporting the same message as text.
type duplicateTagNameError struct {
	name string
}

func (e duplicateTagNameError) Error() string {
	return fmt.Sprintf("a tag named %q already exists", e.name)
}

func agentToolError(text string) *pluginsdk.AgentToolResult {
	return &pluginsdk.AgentToolResult{Text: text, IsError: true}
}
func agentArgString(req *pluginsdk.AgentToolRequest, key string) string {
	value, _ := req.Arguments[key].(string)
	return value
}

func sortedTaskTags(entries []taskTag) []taskTag {
	out := append([]taskTag(nil), entries...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt < out[j].UpdatedAt })
	return out
}
func taskTagView(doc tagDoc, entries []taskTag) []any {
	out := make([]any, 0, len(entries))
	for _, entry := range sortedTaskTags(entries) {
		tag := findTag(doc.Tags, entry.TagID)
		if tag == nil {
			continue
		}
		// The bot distinction describes either agent provenance: a tag created
		// by an agent remains visibly agent-owned even if a person later adds
		// it to another card, and an agent application marks a human-owned
		// definition only while that application remains present.
		out = append(out, map[string]any{"id": tag.ID, "name": tag.Name, "color": tag.Color, "owner": tag.Owner, "agent": entry.Agent || tag.Owner == ownerAgent, "agentApplied": entry.Agent, "human": entry.Human, "note": entry.Note, "updatedAt": entry.UpdatedAt})
	}
	return out
}
func catalogView(doc tagDoc) []any {
	out := make([]any, 0, len(doc.Tags))
	for _, tag := range doc.Tags {
		out = append(out, map[string]any{"id": tag.ID, "name": tag.Name, "color": tag.Color, "owner": tag.Owner, "createdAt": tag.CreatedAt, "updatedAt": tag.UpdatedAt})
	}
	return out
}
func agentResult(text string, doc tagDoc, taskID string) *pluginsdk.AgentToolResult {
	return &pluginsdk.AgentToolResult{Text: text, StructuredContent: map[string]any{"catalog": catalogView(doc), "tags": taskTagView(doc, doc.Tasks[taskID])}}
}
func requireAgentOwnedTag(doc tagDoc, id string) (*sharedTag, error) {
	tag := findTag(doc.Tags, id)
	if tag == nil {
		return nil, fmt.Errorf("tag %q does not exist", id)
	}
	if tag.Owner != ownerAgent {
		return nil, fmt.Errorf("agents can only manage agent-created tags")
	}
	return tag, nil
}

func (p *tagsPlugin) InvokeAgentTool(ctx context.Context, req *pluginsdk.AgentToolRequest) (*pluginsdk.AgentToolResult, error) {
	if msg := requireToolContext(req); msg != "" {
		return agentToolError(msg), nil
	}
	taskID := targetTaskID(req)
	switch req.Name {
	case "create_tag":
		name, err := normalizeTagName(agentArgString(req, "name"))
		if err != nil {
			return agentToolError(err.Error()), nil
		}
		color, err := p.newTagColor(ctx, name, agentArgString(req, "color"))
		if err != nil {
			return agentToolError(err.Error()), nil
		}
		var created sharedTag
		doc, err := p.mutateTagDoc(ctx, req.Context.WorkspaceID, func(doc *tagDoc) error {
			if hasTagName(doc.Tags, name, "") {
				return duplicateTagNameError{name: name}
			}
			id, err := newTagID()
			if err != nil {
				return err
			}
			timestamp := now()
			created = sharedTag{ID: id, Name: name, Color: color, Owner: ownerAgent, CreatedAt: timestamp, UpdatedAt: timestamp}
			doc.Tags = append(doc.Tags, created)
			return nil
		})
		if err != nil {
			return agentToolError(err.Error()), nil
		}
		return agentResult("created agent tag "+created.Name, doc, taskID), nil
	case "update_tag":
		id := agentArgString(req, "tag_id")
		nameArg, colorArg := agentArgString(req, "name"), agentArgString(req, "color")
		// An absent color and an explicitly empty one are different requests: the
		// board's tag-update refuses "" rather than reading it as "leave the color
		// alone", and an agent that sends "" means to change something -- so it gets
		// the same refusal, instead of a silently ignored field and a message about
		// a required color it did pass. Creation keeps the opposite convention
		// (an empty color there means "derive one"), which its description states.
		_, nameSupplied := req.Arguments["name"]
		_, colorSupplied := req.Arguments["color"]
		if id == "" {
			return agentToolError("tag_id is required"), nil
		}
		if !nameSupplied && !colorSupplied {
			return agentToolError("name or color is required"), nil
		}
		// A supplied-but-blank name is refused like a supplied-but-blank color, and
		// like the board: an agent that sends one meant to change the name, and the
		// whole request is refused rather than reported as a success that changed
		// only the color. (The board's tag-update aborts on an empty name too, so
		// the color is not applied there either.)
		if nameSupplied && strings.TrimFunc(nameArg, stripFromEdges) == "" {
			return agentToolError("tag name is required"), nil
		}
		if colorSupplied && strings.TrimFunc(colorArg, stripFromEdges) == "" {
			return agentToolError("color must be a 3- or 6-digit hex value"), nil
		}
		doc, err := p.mutateTagDoc(ctx, req.Context.WorkspaceID, func(doc *tagDoc) error {
			tag, err := requireAgentOwnedTag(*doc, id)
			if err != nil {
				return err
			}
			if nameArg != "" {
				name, err := normalizeTagName(nameArg)
				if err != nil {
					return err
				}
				if hasTagName(doc.Tags, name, id) {
					return duplicateTagNameError{name: name}
				}
				tag.Name = name
			}
			if colorArg != "" {
				color, err := normalizeTagColor(colorArg)
				if err != nil {
					return err
				}
				tag.Color = color
			}
			tag.UpdatedAt = now()
			doc.Tags[findTagIndex(doc.Tags, id)] = *tag
			return nil
		})
		if err != nil {
			return agentToolError(err.Error()), nil
		}
		return agentResult("updated agent tag", doc, taskID), nil
	case "delete_tag":
		id := agentArgString(req, "tag_id")
		if id == "" {
			return agentToolError("tag_id is required"), nil
		}
		doc, err := p.mutateTagDoc(ctx, req.Context.WorkspaceID, func(doc *tagDoc) error {
			if _, err := requireAgentOwnedTag(*doc, id); err != nil {
				return err
			}
			deleteTag(doc, id)
			return nil
		})
		if err != nil {
			return agentToolError(err.Error()), nil
		}
		return agentResult("deleted agent tag", doc, taskID), nil
	case "add_tag":
		id := agentArgString(req, "tag_id")
		if id == "" {
			return agentToolError("tag_id is required"), nil
		}
		note := truncateNote(agentArgString(req, "note"))
		doc, err := p.mutateTagDoc(ctx, req.Context.WorkspaceID, func(doc *tagDoc) error {
			if _, err := requireAgentOwnedTag(*doc, id); err != nil {
				return err
			}
			if err := ensureAgentTaskSlot(*doc, taskID); err != nil {
				return err
			}
			entries := doc.Tasks[taskID]
			i := findTaskTagIndex(entries, id)
			timestamp := now()
			if i < 0 {
				entries = append(entries, taskTag{TagID: id, Agent: true, Note: note, SessionID: req.Context.SessionID, UpdatedAt: timestamp})
			} else {
				entries[i].Agent = true
				entries[i].Note = note
				entries[i].SessionID = req.Context.SessionID
				entries[i].UpdatedAt = timestamp
			}
			doc.Tasks[taskID] = entries
			return nil
		})
		if err != nil {
			return agentToolError(err.Error()), nil
		}
		return agentResult("added agent tag", doc, taskID), nil
	case "remove_tag":
		id := agentArgString(req, "tag_id")
		if id == "" {
			return agentToolError("tag_id is required"), nil
		}
		doc, err := p.mutateTagDoc(ctx, req.Context.WorkspaceID, func(doc *tagDoc) error {
			if _, err := requireAgentOwnedTag(*doc, id); err != nil {
				return err
			}
			removeAgentApplication(doc, taskID, id)
			return nil
		})
		if err != nil {
			return agentToolError(err.Error()), nil
		}
		return agentResult("removed agent tag", doc, taskID), nil
	case "list_tags":
		doc, err := p.readTagDoc(ctx, req.Context.WorkspaceID)
		if err != nil {
			return nil, err
		}
		return agentResult("listed shared tags", doc, taskID), nil
	default:
		return agentToolError("unknown agent tool: " + req.Name), nil
	}
}

func deleteTag(doc *tagDoc, id string) {
	i := findTagIndex(doc.Tags, id)
	if i >= 0 {
		doc.Tags = append(doc.Tags[:i], doc.Tags[i+1:]...)
	}
	for taskID, entries := range doc.Tasks {
		kept := entries[:0]
		for _, entry := range entries {
			if entry.TagID != id {
				kept = append(kept, entry)
			}
		}
		if len(kept) == 0 {
			delete(doc.Tasks, taskID)
		} else {
			doc.Tasks[taskID] = kept
		}
	}
}
func removeAgentApplication(doc *tagDoc, taskID, tagID string) {
	entries := doc.Tasks[taskID]
	i := findTaskTagIndex(entries, tagID)
	if i < 0 {
		return
	}
	entries[i].Agent = false
	entries[i].Note = ""
	entries[i].SessionID = ""
	entries[i].UpdatedAt = now()
	if !entries[i].Human {
		entries = append(entries[:i], entries[i+1:]...)
	}
	if len(entries) == 0 {
		delete(doc.Tasks, taskID)
	} else {
		doc.Tasks[taskID] = entries
	}
}
