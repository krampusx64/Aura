package agent

// native_tools.go — Builds OpenAI-compatible tool schema definitions from the
// AuraGo built-in tool registry plus dynamically loaded skills and custom tools.
// Used when config.Agent.UseNativeFunctions = true.

import (
	"encoding/json"
	"log/slog"

	openai "github.com/sashabaranov/go-openai"

	"aurago/internal/tools"
)

// prop creates a JSON Schema property entry.
func prop(typ, description string) map[string]interface{} {
	return map[string]interface{}{"type": typ, "description": description}
}

// schema builds a standard object schema with required fields.
func schema(properties map[string]interface{}, required ...string) map[string]interface{} {
	s := map[string]interface{}{
		"type":       "object",
		"properties": properties,
	}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

// tool creates an openai.Tool from a name, description, and parameters schema.
func tool(name, description string, params map[string]interface{}) openai.Tool {
	return openai.Tool{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        name,
			Description: description,
			Parameters:  params,
		},
	}
}

// ToolFeatureFlags controls which optional tool schemas are included.
type ToolFeatureFlags struct {
	HomeAssistantEnabled bool
	DockerEnabled        bool
	CoAgentEnabled       bool
}

// builtinToolSchemas returns schemas for all built-in AuraGo tools.
// Optional feature tools (home_assistant, docker, co_agent) are only
// included when their corresponding feature is enabled in the config.
func builtinToolSchemas(ff ToolFeatureFlags) []openai.Tool {
	tools := []openai.Tool{
		tool("execute_shell",
			"Run a shell command on the local system. Use for file operations, system info, running programs, etc.",
			schema(map[string]interface{}{
				"command":    prop("string", "The shell command to execute"),
				"background": prop("boolean", "Run as background process (default false)"),
			}, "command"),
		),
		tool("execute_python",
			"Save and execute a Python script. Use for data processing, automation, calculations, and scripting tasks.",
			schema(map[string]interface{}{
				"code":        prop("string", "The complete Python code to execute"),
				"description": prop("string", "Brief description of what this script does"),
			}, "code"),
		),
		tool("execute_skill",
			"Run a pre-built registered skill (e.g. web_search, ddg_search, pdf_extractor, wikipedia_search, google_workspace, virustotal_scan). Use for external data retrieval.",
			schema(map[string]interface{}{
				"skill": prop("string", "Name of the skill to execute (e.g. 'ddg_search', 'web_scraper', 'pdf_extractor', 'virustotal_scan')"),
				"skill_args": map[string]interface{}{
					"type":        "object",
					"description": "Arguments to pass to the skill as key-value pairs",
				},
			}, "skill"),
		),
		tool("filesystem",
			"Read, write, move, copy, delete files and directories, or list directory contents.",
			schema(map[string]interface{}{
				"operation": map[string]interface{}{
					"type":        "string",
					"description": "Operation to perform",
					"enum":        []string{"read", "write", "append", "delete", "move", "copy", "list", "exists", "mkdir"},
				},
				"file_path":   prop("string", "Path to the file or directory"),
				"content":     prop("string", "Content to write (for write/append operations)"),
				"destination": prop("string", "Destination path (for move/copy operations)"),
				"preview":     prop("boolean", "If true, only return first 100 lines (for read)"),
			}, "operation", "file_path"),
		),
		tool("manage_memory",
			"Store or delete facts in long-term memory, or save structured key-value data to core memory.",
			schema(map[string]interface{}{
				"operation": map[string]interface{}{
					"type":        "string",
					"description": "Operation to perform",
					"enum":        []string{"store", "delete", "save_core", "delete_core"},
				},
				"fact":  prop("string", "A factual statement to store (for 'store' operation)"),
				"key":   prop("string", "Key name for core memory (for 'save_core'/'delete_core')"),
				"value": prop("string", "Value to save for the given key (for 'save_core')"),
			}, "operation"),
		),
		tool("query_memory",
			"Search long-term memory for relevant stored knowledge using a natural language query.",
			schema(map[string]interface{}{
				"query": prop("string", "Natural language search query"),
			}, "query"),
		),
		tool("system_metrics",
			"Retrieve current system resource usage: CPU, memory, disk, running processes.",
			schema(map[string]interface{}{
				"target": map[string]interface{}{
					"type":        "string",
					"description": "Metrics to retrieve",
					"enum":        []string{"all", "cpu", "memory", "disk", "processes"},
				},
			}),
		),
		tool("process_management",
			"List, kill, or inspect running background processes managed by AuraGo.",
			schema(map[string]interface{}{
				"operation": map[string]interface{}{
					"type":        "string",
					"description": "Operation to perform",
					"enum":        []string{"list", "kill", "status"},
				},
				"pid":   prop("integer", "Process ID (for kill/status operations)"),
				"label": prop("string", "Process label (alternative to pid)"),
			}, "operation"),
		),
		tool("knowledge_graph",
			"Store or query relationships between entities in the knowledge graph.",
			schema(map[string]interface{}{
				"operation": map[string]interface{}{
					"type":        "string",
					"description": "Operation to perform",
					"enum":        []string{"add_relation", "query", "delete_relation"},
				},
				"source":     prop("string", "Source entity name"),
				"target":     prop("string", "Target entity name"),
				"relation":   prop("string", "Relationship type (e.g. 'owns', 'is_part_of')"),
				"query":      prop("string", "Natural language query for 'query' operation"),
				"properties": map[string]interface{}{"type": "object", "description": "Optional properties for the relation"},
			}, "operation"),
		),
		tool("remote_execution",
			"Execute a command on a remote SSH server registered in the inventory.",
			schema(map[string]interface{}{
				"server_id": prop("string", "Server ID or hostname from the inventory"),
				"command":   prop("string", "Shell command to run on the remote server"),
				"direction": map[string]interface{}{
					"type":        "string",
					"description": "For file transfer: 'upload' or 'download'",
					"enum":        []string{"upload", "download"},
				},
				"local_path":  prop("string", "Local file path (for file transfer)"),
				"remote_path": prop("string", "Remote file path (for file transfer)"),
			}, "server_id", "command"),
		),
		tool("api_request",
			"Make an HTTP request to an external API endpoint.",
			schema(map[string]interface{}{
				"url":    prop("string", "The full URL to request"),
				"method": map[string]interface{}{"type": "string", "description": "HTTP method", "enum": []string{"GET", "POST", "PUT", "PATCH", "DELETE"}},
				"headers": map[string]interface{}{
					"type":        "object",
					"description": "HTTP headers as key-value string pairs",
				},
				"body": prop("string", "Request body (for POST/PUT/PATCH)"),
			}, "url"),
		),
		tool("secrets_vault",
			"Store, retrieve, list, or delete secrets from the encrypted vault.",
			schema(map[string]interface{}{
				"operation": map[string]interface{}{
					"type":        "string",
					"description": "Vault operation",
					"enum":        []string{"get", "set", "delete", "list"},
				},
				"key":   prop("string", "Secret key name"),
				"value": prop("string", "Secret value (for 'set' operation)"),
			}, "operation"),
		),
		tool("cron_scheduler",
			"Schedule, list, enable, disable, or remove recurring background tasks.",
			schema(map[string]interface{}{
				"operation": map[string]interface{}{
					"type":        "string",
					"description": "Scheduler operation",
					"enum":        []string{"add", "list", "remove", "enable", "disable"},
				},
				"cron_expr":   prop("string", "Cron expression (e.g. '0 9 * * *' for daily at 9am)"),
				"task_prompt": prop("string", "The prompt/task to execute on schedule"),
				"id":          prop("string", "Job ID (for remove/enable/disable)"),
				"label":       prop("string", "Human-readable label for the job"),
			}, "operation"),
		),
		tool("save_tool",
			"Save a new Python tool/script to the tools directory and register it in the manifest.",
			schema(map[string]interface{}{
				"name":        prop("string", "Filename for the tool (e.g. 'my_tool.py')"),
				"description": prop("string", "What this tool does"),
				"code":        prop("string", "Complete Python code for the tool"),
			}, "name", "description", "code"),
		),
		tool("follow_up",
			"Schedule a follow-up agent invocation with a specific task prompt (for multi-step async tasks).",
			schema(map[string]interface{}{
				"task_prompt": prop("string", "The task prompt for the follow-up invocation"),
			}, "task_prompt"),
		),
		tool("manage_notes",
			"Create, list, update, toggle, or delete persistent notes and to-do items.",
			schema(map[string]interface{}{
				"operation": map[string]interface{}{
					"type":        "string",
					"description": "Notes operation",
					"enum":        []string{"add", "list", "update", "toggle", "delete"},
				},
				"title":    prop("string", "Title of the note (required for add)"),
				"content":  prop("string", "Detailed content or body text"),
				"category": prop("string", "Category tag (e.g. 'todo', 'ideas', 'shopping'). Default: 'general'"),
				"priority": prop("integer", "Priority: 1=low, 2=medium (default), 3=high"),
				"due_date": prop("string", "Due date in YYYY-MM-DD format"),
				"note_id":  prop("integer", "Note ID (required for update/toggle/delete)"),
				"done":     prop("integer", "Filter for list: -1=all, 0=open only, 1=done only"),
			}, "operation"),
		),
		tool("analyze_image",
			"Analyze an image file using the Vision LLM. Describe content, read text (OCR), identify objects.",
			schema(map[string]interface{}{
				"file_path": prop("string", "Path to the image file (JPEG, PNG, GIF, WebP)"),
				"prompt":    prop("string", "Custom analysis prompt (default: general description)"),
			}, "file_path"),
		),
		tool("transcribe_audio",
			"Transcribe an audio file to text using the configured Speech-to-Text service.",
			schema(map[string]interface{}{
				"file_path": prop("string", "Path to the audio file (MP3, WAV, OGG, FLAC, M4A)"),
			}, "file_path"),
		),
	}

	if ff.HomeAssistantEnabled {
		tools = append(tools, tool("home_assistant",
			"Control Home Assistant smart-home devices: get entity states, call services (turn on/off lights, switches, scenes, etc.), and list available services.",
			schema(map[string]interface{}{
				"operation": map[string]interface{}{
					"type":        "string",
					"description": "Operation to perform",
					"enum":        []string{"get_states", "get_state", "call_service", "list_services"},
				},
				"entity_id": prop("string", "Entity ID (e.g. 'light.living_room', 'switch.heater')"),
				"domain":    prop("string", "HA domain for filtering or service calls (e.g. 'light', 'switch', 'climate', 'scene')"),
				"service":   prop("string", "Service to call (e.g. 'turn_on', 'turn_off', 'toggle')"),
				"service_data": map[string]interface{}{
					"type":        "object",
					"description": "Additional parameters for the service call (e.g. brightness, temperature, color)",
				},
			}, "operation"),
		))
	}

	if ff.DockerEnabled {
		tools = append(tools, tool("docker",
			"Manage Docker containers, images, networks, and volumes. List, inspect, start, stop, create, remove containers; pull/remove images; view logs; get system info.",
			schema(map[string]interface{}{
				"operation": map[string]interface{}{
					"type":        "string",
					"description": "Operation to perform",
					"enum":        []string{"list_containers", "inspect", "start", "stop", "restart", "pause", "unpause", "remove", "logs", "create", "run", "list_images", "pull", "remove_image", "list_networks", "list_volumes", "info"},
				},
				"container_id": prop("string", "Container ID or name (for container operations)"),
				"image":        prop("string", "Docker image name with optional tag (e.g. 'nginx:latest')"),
				"name":         prop("string", "Container name (for create/run)"),
				"command":      prop("string", "Command to run in the container"),
				"env":          map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "Environment variables (e.g. ['KEY=value'])"},
				"ports":        map[string]interface{}{"type": "object", "description": "Port mappings: {'container_port': 'host_port'} (e.g. {'80': '8080'})"},
				"volumes":      map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "Volume binds (e.g. ['/host/path:/container/path'])"},
				"restart":      prop("string", "Restart policy: no, always, unless-stopped, on-failure"),
				"force":        prop("boolean", "Force removal (for remove/remove_image)"),
				"tail":         prop("integer", "Number of log lines to return (default: 100)"),
				"all":          prop("boolean", "Include stopped containers (for list_containers)"),
			}, "operation"),
		))
	}

	if ff.CoAgentEnabled {
		tools = append(tools, tool("co_agent",
			"Spawn and manage parallel co-agents that work on sub-tasks independently. Co-agents run in background goroutines with their own LLM context and return results when done.",
			schema(map[string]interface{}{
				"operation": map[string]interface{}{
					"type":        "string",
					"description": "Operation to perform",
					"enum":        []string{"spawn", "list", "get_result", "stop", "stop_all"},
				},
				"task":          prop("string", "Task description for the co-agent to work on (required for 'spawn')"),
				"co_agent_id":   prop("string", "Co-agent ID (required for 'get_result' and 'stop')"),
				"context_hints": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "Optional keywords or topics for RAG context injection (for 'spawn')"},
			}, "operation"),
		))
	}

	tools = append(tools, tool("manage_updates",
		"Check for AuraGo updates on GitHub or install them. Use 'check' to see if a new version is available without installing. Use 'install' only after user approval.",
		schema(map[string]interface{}{
			"operation": map[string]interface{}{
				"type":        "string",
				"description": "Operation: 'check' (dry run) or 'install' (applies updates)",
				"enum":        []string{"check", "install"},
			},
		}, "operation"),
	))

	return tools
}

// NativeToolCallToToolCall converts an OpenAI native ToolCall response to AuraGo's ToolCall struct.
// Arguments JSON is unmarshalled directly into the struct fields.
func NativeToolCallToToolCall(native openai.ToolCall, logger *slog.Logger) ToolCall {
	tc := ToolCall{
		IsTool: true,
		Action: native.Function.Name,
	}

	if native.Function.Arguments == "" {
		return tc
	}

	// Unmarshal the arguments JSON into the ToolCall struct
	if err := json.Unmarshal([]byte(native.Function.Arguments), &tc); err != nil {
		if logger != nil {
			logger.Warn("[NativeTools] Failed to unmarshal native tool arguments, using raw",
				"name", native.Function.Name, "error", err)
		}
		// Fallback: try to put the raw args into Params
		var rawMap map[string]interface{}
		if json.Unmarshal([]byte(native.Function.Arguments), &rawMap) == nil {
			tc.Params = rawMap
		}
		return tc
	}

	// Ensure action is set correctly (unmarshal may overwrite it if the LLM included it)
	if tc.Action == "" {
		tc.Action = native.Function.Name
	}

	// Handle execute_skill: LLM may use "skill_name" key
	if tc.Action == "execute_skill" && tc.Skill == "" {
		for _, key := range []string{"skill_name", "name", "skill_name"} {
			if tc.Params != nil {
				if v, ok := tc.Params[key].(string); ok && v != "" {
					tc.Skill = v
					break
				}
			}
		}
	}

	return tc
}

// BuildNativeToolSchemas returns the full tool list: built-ins + registered skills + custom tools.
func BuildNativeToolSchemas(skillsDir string, manifest *tools.Manifest, enableGoogleWorkspace bool, ff ToolFeatureFlags, logger *slog.Logger) []openai.Tool {
	allTools := builtinToolSchemas(ff)

	// Add skills as sub-variants of execute_skill (informational context; already handled by execute_skill schema)
	if skills, err := tools.ListSkills(skillsDir, enableGoogleWorkspace); err == nil {
		for _, skill := range skills {
			allTools = append(allTools, tool(
				"skill__"+skill.Name,
				"(Skill) "+skill.Description+". Use execute_skill with skill='"+skill.Name+"'.",
				schema(map[string]interface{}{
					"skill_args": map[string]interface{}{
						"type":        "object",
						"description": "Arguments for this skill",
					},
				}),
			))
		}
	}

	// Add custom tools from manifest
	if manifest != nil {
		if entries, err := manifest.Load(); err == nil {
			for name, description := range entries {
				allTools = append(allTools, tool(
					"tool__"+name,
					"(Custom tool) "+description,
					schema(map[string]interface{}{
						"params": map[string]interface{}{
							"type":        "object",
							"description": "Parameters to pass to the tool",
						},
					}),
				))
			}
		}
	}

	if logger != nil {
		logger.Debug("[NativeTools] Built tool schemas", "count", len(allTools))
	}

	return allTools
}
