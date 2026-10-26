package bifrost

import (
	"strings"

	"github.com/plutus-cloud/plutus-collector/internal/config"
)

// AuthFromConfig builds the management-API credentials from validated configuration.
func AuthFromConfig(cfg *config.BifrostConfig) Auth {
	a := Auth{BasicUser: cfg.BasicUser, BasicPass: cfg.BasicPass}
	if cfg.APIKey != "" {
		a.Header = cfg.APIKeyHeader
		a.Value = strings.TrimSpace(cfg.APIKeyScheme + " " + cfg.APIKey)
	}
	return a
}

// FieldsFromConfig applies BIFROST_FIELD_<NAME> overrides on top of the source-derived defaults.
func FieldsFromConfig(cfg *config.BifrostConfig) FieldMap {
	f := DefaultFields()
	for name, paths := range cfg.FieldOverrides {
		switch name {
		case "TIMESTAMP":
			f.Timestamp = paths
		case "STATUS":
			f.Status = paths
		case "PROVIDER":
			f.Provider = paths
		case "MODEL":
			f.Model = paths
		case "VIRTUAL_KEY":
			f.VirtualKey = paths
		case "TEAM":
			f.Team = paths
		case "COST":
			f.Cost = paths
		case "INPUT_TOKENS":
			f.InputTokens = paths
		case "OUTPUT_TOKENS":
			f.OutputTokens = paths
		}
	}
	return f
}
