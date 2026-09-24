// Package architecture enforces the dependency direction of the
// modularization programme (llm-gateway#67): code destined for the shared
// libraries must not gain dependencies on the gateway's storage,
// configuration or HTTP layer.
package architecture
