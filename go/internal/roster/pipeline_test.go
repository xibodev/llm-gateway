package roster

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// Pin the producer/consumer wire contract with the real Node collector and signer.
func TestNodePipelineContract(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node is required for the cross-language pipeline check")
	}
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
	out := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("node", args...)
		cmd.Dir = root
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("pipeline stage failed: %v\n%s", err, output)
		}
	}
	run("scripts/provider-roster/build.mjs", "--output", out, "--fixtures", "scripts/provider-roster/fixtures", "--offline")
	// A private key exists only inside this process and is never written to disk.
	script := `const fs=require('node:fs');const path=require('node:path');const {pathToFileURL}=require('node:url');(async()=>{const {signStaging}=await import(pathToFileURL(path.resolve('scripts/provider-roster-community/sign.mjs')));const result=signStaging(fs.readFileSync(path.join(process.argv[1],'payload.json')));fs.writeFileSync(path.join(process.argv[1],'signed.json'),JSON.stringify(result));})().catch(()=>process.exit(1));`
	run("-e", script, out)
	data, err := os.ReadFile(filepath.Join(out, "signed.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Envelope json.RawMessage `json:"envelope"`
		Trust    struct {
			KeyID     string `json:"key_id"`
			PublicKey string `json:"public_key"`
		} `json:"trust"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	key, err := base64.StdEncoding.DecodeString(result.Trust.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	payload, _, err := verify(result.Envelope, map[string]ed25519.PublicKey{result.Trust.KeyID: key}, time.Now())
	if err != nil {
		t.Fatalf("Go rejected real collector/signer output: %v", err)
	}
	if len(payload.Entries) == 0 || len(payload.Sources) != 4 {
		t.Fatalf("missing pipeline entries or sources: %d / %d", len(payload.Entries), len(payload.Sources))
	}
	for _, source := range payload.Sources {
		if source.License == "" {
			t.Fatalf("source license metadata lost: %s", source.Repo)
		}
	}
}
