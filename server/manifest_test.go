package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

type testManifest struct {
	ID           string         `yaml:"id"`
	Version      string         `yaml:"version"`
	ConfigSchema map[string]any `yaml:"config_schema"`
	Capabilities struct {
		State     bool `yaml:"state"`
		UserState bool `yaml:"user_state"`
	} `yaml:"capabilities"`
	Actions []struct {
		Key   string `yaml:"key"`
		Scope string `yaml:"scope"`
	} `yaml:"actions"`
	AgentTools []struct {
		Name        string   `yaml:"name"`
		Description string   `yaml:"description"`
		Surfaces    []string `yaml:"surfaces"`
		InputSchema struct {
			Type       string                    `yaml:"type"`
			Required   []string                  `yaml:"required"`
			Properties map[string]map[string]any `yaml:"properties"`
		} `yaml:"input_schema"`
	} `yaml:"agent_tools"`
}

func loadTestManifest(t *testing.T) testManifest {
	t.Helper()
	data, err := os.ReadFile("../manifest.yaml")
	if os.IsNotExist(err) {
		data, err = os.ReadFile("manifest.yaml")
	}
	require.NoError(t, err)
	var manifest testManifest
	require.NoError(t, yaml.Unmarshal(data, &manifest))
	return manifest
}

func TestManifestDeclaresSharedCatalogContract(t *testing.T) {
	manifest := loadTestManifest(t)
	// The release workflow updates the manifest version before it runs the
	// verification suite, so this contract test must validate the version
	// format rather than pinning a particular release number.
	require.Regexp(t, `^[0-9]+\.[0-9]+\.[0-9]+$`, manifest.Version)
	// These values address persisted workspace/private state. Changing the id
	// or dropping either capability makes existing tags inaccessible.
	require.Equal(t, "kandev-plugin-tags", manifest.ID)
	require.True(t, manifest.Capabilities.State)
	require.True(t, manifest.Capabilities.UserState)
	require.Equal(t, []string{"shared-tags", "tag-create", "tag-update", "tag-delete", "task-tag-add", "task-tag-remove"}, func() []string {
		out := make([]string, len(manifest.Actions))
		for i := range manifest.Actions {
			out[i] = manifest.Actions[i].Key
		}
		return out
	}())
	require.Equal(t, []string{"workspace", "workspace", "workspace", "workspace", "task", "task"}, func() []string {
		out := make([]string, len(manifest.Actions))
		for i := range manifest.Actions {
			out[i] = manifest.Actions[i].Scope
		}
		return out
	}())
	require.Equal(t, []string{"create_tag", "update_tag", "delete_tag", "add_tag", "remove_tag", "list_tags"}, func() []string {
		out := make([]string, len(manifest.AgentTools))
		for i := range manifest.AgentTools {
			out[i] = manifest.AgentTools[i].Name
			require.Equal(t, []string{"kanban-task"}, manifest.AgentTools[i].Surfaces)
			require.Equal(t, "object", manifest.AgentTools[i].InputSchema.Type)
		}
		return out
	}())
}

// The host validates arguments against these schemas and forces
// additionalProperties=false, so an undeclared task_id would be rejected before
// it ever reached the plugin: this contract is what makes cross-task tagging
// reachable at all.
func TestManifestDeclaresOptionalTaskIDOnTaskScopedAgentTools(t *testing.T) {
	manifest := loadTestManifest(t)
	tools := map[string]int{}
	for i := range manifest.AgentTools {
		tools[manifest.AgentTools[i].Name] = i
	}

	// Tools that address a single task's applications accept an optional
	// task_id; it stays out of `required` so omitting it keeps today's
	// caller's-own-task behaviour.
	for _, name := range []string{"add_tag", "remove_tag", "list_tags"} {
		require.Contains(t, tools, name, name+" must be declared in the manifest")
		tool := manifest.AgentTools[tools[name]]
		require.Contains(t, tool.InputSchema.Properties, "task_id", name)
		require.Equal(t, "string", tool.InputSchema.Properties["task_id"]["type"], name)
		require.NotContains(t, tool.InputSchema.Required, "task_id", name+" must keep task_id optional")
		require.NotContains(t, tool.Description, "the current task",
			name+" description must not claim it only acts on the current task")
	}

	// Catalog tools operate on the workspace-wide tag list, not on any one
	// task's applications, so they deliberately take no task_id.
	for _, name := range []string{"create_tag", "update_tag", "delete_tag"} {
		// Without this guard a missing name would index 0 (create_tag, which has
		// no task_id) and the NotContains below would pass vacuously.
		require.Contains(t, tools, name, name+" must be declared in the manifest")
		tool := manifest.AgentTools[tools[name]]
		require.NotContains(t, tool.InputSchema.Properties, "task_id",
			name+" acts on the catalog, not a task")
	}
}

// An agent reads these schemas, not the README: the host validates input against
// them before the plugin ever runs, so a color field with no stated format turns
// a preventable mistake into an invocation error the agent has to recover from.
func TestManifestStatesTheAgentColorFormat(t *testing.T) {
	manifest := loadTestManifest(t)
	var checked int
	for i := range manifest.AgentTools {
		tool := manifest.AgentTools[i]
		property, ok := tool.InputSchema.Properties["color"]
		if !ok {
			continue
		}
		checked++
		require.Equal(t, "string", property["type"], tool.Name)
		require.Equal(t, `^#[0-9a-fA-F]{3}([0-9a-fA-F]{3})?$`, property["pattern"], tool.Name)
		require.Contains(t, property["description"].(string), "#rgb", tool.Name)
	}
	require.Equal(t, 2, checked, "create_tag and update_tag both take a color")
}

// The settings form at Settings > Plugins > Tags is generated from this
// schema, and the plugin reads the saved values through Host.GetConfig keyed by
// the same property name (tagColorSettingKey), so a renamed or retyped property
// would leave the operator's toggle accepted-but-ignored -- silently deriving
// colors they turned off.
func TestManifestDeclaresAutoColorSetting(t *testing.T) {
	manifest := loadTestManifest(t)
	properties, ok := manifest.ConfigSchema["properties"].(map[string]any)
	require.True(t, ok, "config_schema.properties must be declared for the settings form to exist")

	setting, ok := properties[tagColorSettingKey].(map[string]any)
	require.True(t, ok, "config_schema must declare the property the plugin reads")
	require.Equal(t, "boolean", setting["type"])
	// The backend serves only saved values, so the behavior behind this
	// declaration comes from the plugin's own fallback (pinned by
	// TestAutoColorSettingControlsDerivedColor). `default` is not decorative
	// either: the web settings form seeds an unset boolean from it
	// (apps/web/lib/plugins/config-schema.ts buildInitialValues falls back to
	// false when no default is declared), so dropping it would render the
	// Switch off while the plugin kept deriving colors -- a visible control
	// that says the opposite of what happens.
	require.Equal(t, true, setting["default"])
}
