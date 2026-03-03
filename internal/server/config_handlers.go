package server

import (
	"aurago/internal/budget"
	"aurago/internal/config"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// handleGetConfig returns the current config as JSON with sensitive fields masked.
func handleGetConfig(s *Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		configPath := s.Cfg.ConfigPath
		if configPath == "" {
			http.Error(w, "Config path not set", http.StatusInternalServerError)
			return
		}

		data, err := os.ReadFile(configPath)
		if err != nil {
			s.Logger.Error("Failed to read config file", "error", err)
			http.Error(w, "Failed to read config", http.StatusInternalServerError)
			return
		}

		var rawCfg map[string]interface{}
		if err := yaml.Unmarshal(data, &rawCfg); err != nil {
			s.Logger.Error("Failed to parse config", "error", err)
			http.Error(w, "Failed to parse config", http.StatusInternalServerError)
			return
		}

		// Mask sensitive fields
		maskSensitiveFields(rawCfg)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(rawCfg)
	}
}

// handleUpdateConfig patches the config.yaml with the provided JSON values.
func handleUpdateConfig(s *Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		configPath := s.Cfg.ConfigPath
		if configPath == "" {
			http.Error(w, "Config path not set", http.StatusInternalServerError)
			return
		}

		// Read the incoming patch (with size limit to prevent OOM)
		maxBody := s.Cfg.Server.MaxBodyBytes
		if maxBody <= 0 {
			maxBody = 10 << 20 // 10 MB default
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Failed to read request body", http.StatusBadRequest)
			return
		}
		defer r.Body.Close()

		var patch map[string]interface{}
		if err := json.Unmarshal(body, &patch); err != nil {
			http.Error(w, "Invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}

		// Read the current config
		data, err := os.ReadFile(configPath)
		if err != nil {
			s.Logger.Error("Failed to read config file for patching", "error", err)
			http.Error(w, "Failed to read config", http.StatusInternalServerError)
			return
		}

		var rawCfg map[string]interface{}
		if err := yaml.Unmarshal(data, &rawCfg); err != nil {
			s.Logger.Error("Failed to parse config for patching", "error", err)
			http.Error(w, "Failed to parse config", http.StatusInternalServerError)
			return
		}

		// Deep merge the patch into the existing config, skipping masked password values
		deepMerge(rawCfg, patch)

		// Write back
		out, err := yaml.Marshal(rawCfg)
		if err != nil {
			s.Logger.Error("Failed to marshal patched config", "error", err)
			http.Error(w, "Failed to save config", http.StatusInternalServerError)
			return
		}

		if err := os.WriteFile(configPath, out, 0644); err != nil {
			s.Logger.Error("Failed to write config file", "error", err)
			http.Error(w, "Failed to write config", http.StatusInternalServerError)
			return
		}

		// Hot-reload: re-parse config and apply to running instance
		s.CfgMu.Lock()
		oldCfg := *s.Cfg // snapshot before reload
		newCfg, loadErr := config.Load(configPath)

		needsRestart := false
		restartReasons := []string{}

		if loadErr != nil {
			s.Logger.Warn("[Config UI] Hot-reload failed, changes saved but require restart", "error", loadErr)
			needsRestart = true
			restartReasons = append(restartReasons, "Parse-Fehler beim Reload")
		} else {
			// Detect sections that need restart
			if oldCfg.Server != newCfg.Server {
				needsRestart = true
				restartReasons = append(restartReasons, "Server (Host/Port)")
			}
			if oldCfg.Telegram != newCfg.Telegram {
				needsRestart = true
				restartReasons = append(restartReasons, "Telegram")
			}
			if oldCfg.Discord != newCfg.Discord {
				needsRestart = true
				restartReasons = append(restartReasons, "Discord")
			}
			if oldCfg.SQLite != newCfg.SQLite {
				needsRestart = true
				restartReasons = append(restartReasons, "Datenbanken")
			}
			if oldCfg.Directories != newCfg.Directories {
				needsRestart = true
				restartReasons = append(restartReasons, "Verzeichnisse")
			}
			if oldCfg.Chromecast != newCfg.Chromecast {
				needsRestart = true
				restartReasons = append(restartReasons, "Chromecast/TTS Server")
			}

			// Apply hot-reload: copy all new fields into the live config pointer
			savedPath := s.Cfg.ConfigPath
			*s.Cfg = *newCfg
			s.Cfg.ConfigPath = savedPath

			// Always re-create BudgetTracker after a config reload so that
			// toggling budget.enabled or changing limits takes effect immediately.
			s.BudgetTracker = budget.NewTracker(newCfg, s.Logger, newCfg.Directories.DataDir)
			if newCfg.Budget.Enabled {
				s.Logger.Info("[Config UI] BudgetTracker re-initialized", "enabled", true)
			} else {
				s.Logger.Info("[Config UI] BudgetTracker disabled")
			}

			s.Logger.Info("[Config UI] Configuration hot-reloaded successfully")
		}
		s.CfgMu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if needsRestart {
			msg := fmt.Sprintf("Gespeichert. Neustart nötig für: %s", strings.Join(restartReasons, ", "))
			json.NewEncoder(w).Encode(map[string]interface{}{
				"status":         "saved",
				"message":        msg,
				"needs_restart":  true,
				"restart_reason": restartReasons,
			})
		} else {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"status":        "saved",
				"message":       "Konfiguration gespeichert und sofort angewendet.",
				"needs_restart": false,
			})
		}
	}
}

// sensitiveKeys are YAML keys whose values should be masked in the API response.
var sensitiveKeys = map[string]bool{
	"api_key":      true,
	"bot_token":    true,
	"password":     true,
	"app_password": true,
	"access_token": true,
	"token":        true,
	"user_key":     true,
	"app_token":    true,
}

// maskSensitiveFields recursively masks sensitive string values in a config map.
func maskSensitiveFields(m map[string]interface{}) {
	for key, val := range m {
		switch v := val.(type) {
		case map[string]interface{}:
			maskSensitiveFields(v)
		case string:
			if sensitiveKeys[key] && v != "" {
				m[key] = "••••••••"
			}
		}
	}
}

// deepMerge recursively merges src into dst. Masked values ("••••••••") and empty
// strings for sensitive fields are skipped to avoid overwriting real secrets.
func deepMerge(dst, src map[string]interface{}) {
	for key, srcVal := range src {
		switch sv := srcVal.(type) {
		case map[string]interface{}:
			// Recurse into nested maps
			if dstMap, ok := dst[key].(map[string]interface{}); ok {
				deepMerge(dstMap, sv)
			} else {
				dst[key] = srcVal
			}
		case []interface{}:
			// JSON arrays: only accept if all elements are valid (not JS stringified objects)
			valid := true
			for _, elem := range sv {
				if s, ok := elem.(string); ok && strings.HasPrefix(s, "[object") {
					valid = false
					break
				}
			}
			if valid {
				dst[key] = srcVal
			}
		case string:
			// Skip masked or empty password fields
			if sensitiveKeys[key] && (sv == "••••••••" || sv == "") {
				continue
			}
			// Skip JavaScript-stringified values like "[object Object]"
			if strings.HasPrefix(sv, "[object") {
				continue
			}
			dst[key] = srcVal
		default:
			dst[key] = srcVal
		}
	}
}

// getConfigSchema returns a JSON schema describing the config structure for the UI.
// It reflects the Config struct to produce field metadata (type, yaml key).
func handleGetConfigSchema(s *Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		schema := buildSchema(reflect.TypeOf(*s.Cfg), "")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(schema)
	}
}

// SchemaField describes a single config field for the UI renderer.
type SchemaField struct {
	Key       string        `json:"key"`
	YAMLKey   string        `json:"yaml_key"`
	Type      string        `json:"type"` // "string", "int", "float", "bool", "object", "array"
	Sensitive bool          `json:"sensitive,omitempty"`
	Children  []SchemaField `json:"children,omitempty"`
}

func buildSchema(t reflect.Type, prefix string) []SchemaField {
	var fields []SchemaField

	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		yamlTag := f.Tag.Get("yaml")
		if yamlTag == "-" || yamlTag == "" {
			// Fields with yaml:"-" are excluded from config file but may need special handling
			yamlTag = strings.ToLower(f.Name)
		}
		// Strip tag options
		if idx := strings.Index(yamlTag, ","); idx >= 0 {
			yamlTag = yamlTag[:idx]
		}

		fullKey := yamlTag
		if prefix != "" {
			fullKey = prefix + "." + yamlTag
		}

		sf := SchemaField{
			Key:     fullKey,
			YAMLKey: yamlTag,
		}

		ft := f.Type
		if ft.Kind() == reflect.Struct {
			sf.Type = "object"
			sf.Children = buildSchema(ft, fullKey)
		} else if ft.Kind() == reflect.Slice {
			sf.Type = "array"
		} else if ft.Kind() == reflect.Bool {
			sf.Type = "bool"
		} else if ft.Kind() == reflect.Int || ft.Kind() == reflect.Int64 || ft.Kind() == reflect.Int32 {
			sf.Type = "int"
		} else if ft.Kind() == reflect.Float64 || ft.Kind() == reflect.Float32 {
			sf.Type = "float"
		} else {
			sf.Type = "string"
		}

		// Mark sensitive fields
		if sensitiveKeys[yamlTag] || f.Tag.Get("yaml") == "-" {
			sf.Sensitive = true
		}

		fields = append(fields, sf)
	}

	return fields
}

// handleRestart triggers an application restart by exiting with code 42
func handleRestart(s *Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		s.Logger.Info("[Config UI] Restart requested via Web UI")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "ok",
			"message": "AuraGo wird neu gestartet...",
		})

		// Exit asynchronously to allow the response to flush
		go func() {
			os.Exit(42) // 42 triggers the loop in start.bat and systemd Restart=on-failure
		}()
	}
}
