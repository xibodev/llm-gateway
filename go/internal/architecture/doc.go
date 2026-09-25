// Package architecture enforces the dependency direction of the
// modularization programme (llm-gateway#67): code destined for the shared
// libraries must not gain dependencies on the gateway's storage,
// configuration or HTTP layer. It also keeps the provider stack's and the
// router's mutable state in their Runtime values rather than in package
// variables (llm-gateway#71).
package architecture
