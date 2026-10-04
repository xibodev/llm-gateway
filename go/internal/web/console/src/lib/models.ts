// upstreamModelID drops the gateway's "provider/" prefix from a catalog model
// id. Provider ids cannot contain "/", so the first slash ends the prefix and
// everything after it is the provider's own id, which may itself be
// namespaced, as in "openrouter/anthropic/claude-x".
export function upstreamModelID(catalogID: string): string {
  const slash = catalogID.indexOf("/");
  return slash > 0 ? catalogID.slice(slash + 1) : catalogID;
}

// verifyModelChoices lists each upstream model once: instances of one tile can
// serve the same model, and repeated option values make a select ambiguous.
export function verifyModelChoices(catalogIDs: string[]): string[] {
  return [...new Set(catalogIDs.map(upstreamModelID).filter(Boolean))];
}
