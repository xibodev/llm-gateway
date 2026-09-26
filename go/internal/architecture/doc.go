// Package architecture enforces the dependency direction the modularization
// programme ended with (llm-gateway#67, llm-gateway#75). The gateway builds
// on released llmgw-core, llm-provider-auth and llm-translate modules, pinned
// in go.mod without replace directives, and uses none of the core APIs it has
// replaced. Its product adapter layer, where the gateway's settings, IAM and
// stores meet llmgw-core, imports only the product layers each unit lists,
// within coupling budgets that only shrink. The package also keeps the
// provider stack's and the router's mutable state in their Runtime values
// rather than in package variables (llm-gateway#71), and keeps published
// settings from being written in place.
package architecture
