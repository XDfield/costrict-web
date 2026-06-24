// Package tools provides a pluggable tool system for AI event handling.
//
// Tools are registered with the Registry and can be executed by name.
// The package avoids importing from the parent clawagent package to
// prevent circular dependencies — runtime dependencies are provided
// through the ToolContext struct.
package tools

import (
	"context"
	"fmt"
)

// Definition describes a tool for LLM tool calling.
type Definition struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// Context holds runtime dependencies for tool execution.
// Created by the caller (agent.go) and passed through Registry.Execute.
type Context struct {
	DeviceID      string
	Directory     string
	SessionID     string
	DeviceProxy   DeviceProxy
	MarkProcessed func()
}

// DeviceProxy is the interface for communicating with device-side services.
type DeviceProxy interface {
	ReplyPermission(ctx context.Context, deviceID, permissionID string, approved bool, directory string) error
	ReplyQuestion(ctx context.Context, deviceID, questionID string, answers [][]string, directory string) error
	GetSessionInfo(ctx context.Context, deviceID, sessionID, directory string) (map[string]any, error)
	GetRecentMessages(ctx context.Context, deviceID, sessionID, directory string, limit int) ([]map[string]any, error)
}

// Tool is the interface that all event tools implement.
type Tool interface {
	Name() string
	Definition() Definition
	Execute(ctx context.Context, argsJSON string, toolCtx *Context) (string, error)
}

// Registry manages registration and look up of tools.
type Registry struct {
	tools map[string]Tool
}

// NewRegistry creates an empty tool registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register adds a tool to the registry.
// Panics if a tool with the same name is already registered.
func (r *Registry) Register(t Tool) {
	name := t.Name()
	if _, ok := r.tools[name]; ok {
		panic(fmt.Sprintf("tool %q already registered", name))
	}
	r.tools[name] = t
}

// GetByName returns a registered tool by name.
func (r *Registry) GetByName(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// All returns all registered tools.
func (r *Registry) All() []Tool {
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	return out
}

// Execute dispatches to a tool by name.
// Returns an error if the tool is not found.
func (r *Registry) Execute(ctx context.Context, name, argsJSON string, toolCtx *Context) (string, error) {
	t, ok := r.tools[name]
	if !ok {
		return "", fmt.Errorf("unknown tool: %s", name)
	}
	return t.Execute(ctx, argsJSON, toolCtx)
}
