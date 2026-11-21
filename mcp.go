package main

import (
	"fmt"
	"net/http"

	"github.com/gvalkov/tailon/internal/logquery"
	"github.com/gvalkov/tailon/internal/mcpserver"
	"github.com/mitchellh/mapstructure"
	"github.com/pelletier/go-toml"
)

var (
	mcpHandler http.Handler
	mcpServer  *mcpserver.Server
)

// parseMCPConfig decodes the optional [mcp] table on top of the defaults. Any
// field that is not present keeps its default value.
func parseMCPConfig(tree *toml.Tree) (mcpserver.Config, error) {
	cfg := mcpserver.DefaultConfig()
	if tree == nil {
		return cfg, nil
	}
	decoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		Result:     &cfg,
		TagName:    "mapstructure",
		DecodeHook: mapstructure.StringToTimeDurationHookFunc(),
	})
	if err != nil {
		return cfg, err
	}
	if err := decoder.Decode(tree.ToMap()); err != nil {
		return cfg, fmt.Errorf("invalid [mcp] configuration: %w", err)
	}
	return cfg, nil
}

// initMCP validates the MCP configuration and creates the HTTP handler. It is
// only called when MCP is enabled.
func initMCP() error {
	if err := config.MCP.Validate(config.RelativeRoot); err != nil {
		return err
	}
	source := func() []logquery.Spec {
		specs := make([]logquery.Spec, 0, len(config.FileSpecs))
		for _, spec := range config.FileSpecs {
			specs = append(specs, logquery.Spec{
				Path:  spec.Path,
				Type:  spec.Type,
				Alias: spec.Alias,
				Group: spec.Group,
			})
		}
		return specs
	}
	server, err := mcpserver.New(source, config.MCP)
	if err != nil {
		return fmt.Errorf("MCP configuration error: %w", err)
	}
	mcpServer = server
	mcpHandler = server.Handler()
	server.LogStartup()
	return nil
}
