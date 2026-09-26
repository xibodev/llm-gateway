package roster

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/png"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestNodeICOLogoPipelineContract(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node is required for the cross-language pipeline check")
	}
	var raster bytes.Buffer
	if err := png.Encode(&raster, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	// Feed a synthetic ICO through the real collector and signer: the signed wire
	// image must be the extracted PNG, while provenance still names the ICO URL.
	script := `
import { buildRoster } from './scripts/provider-roster/index.mjs';
import { collectLogos } from './scripts/provider-roster/logos.mjs';
import { stableID } from './scripts/provider-roster/normalize.mjs';
import { signStaging } from './scripts/provider-roster-community/sign.mjs';
const png = Buffer.from(process.argv[1], 'base64');
const ico = Buffer.alloc(22 + png.length);
ico.writeUInt16LE(1, 2); ico.writeUInt16LE(1, 4);
ico[6] = 2; ico[7] = 2;
ico.writeUInt16LE(1, 10); ico.writeUInt16LE(32, 12);
ico.writeUInt32LE(png.length, 14); ico.writeUInt32LE(22, 18);
png.copy(ico, 22);
const { payload } = await buildRoster({ fixtures: 'scripts/provider-roster/fixtures', offline: true });
const entry = payload.entries[0];
entry.base_url = 'https://api.example.com/v1';
entry.id = stableID(entry.protocol, entry.base_url);
entry.signup_url = 'https://huggingface.co/join';
delete entry.logo;
await collectLogos([entry], async url => {
  if (url !== 'https://huggingface.co/favicon.ico') throw new Error('Unexpected asset URL');
  return { status: 200, body: ico };
});
if (!entry.logo) throw new Error('ICO logo was not collected');
process.stdout.write(JSON.stringify(signStaging(Buffer.from(JSON.stringify(payload)))));
`
	_, file, _, _ := runtime.Caller(0)
	cmd := exec.Command("node", "--input-type=module", "-e", script, base64.StdEncoding.EncodeToString(raster.Bytes()))
	cmd.Dir = filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ICO collector/signer failed: %v\n%s", err, output)
	}
	var result struct {
		Envelope json.RawMessage `json:"envelope"`
		Trust    struct {
			KeyID     string `json:"key_id"`
			PublicKey string `json:"public_key"`
		} `json:"trust"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatal(err)
	}
	key, err := base64.StdEncoding.DecodeString(result.Trust.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	payload, _, err := verify(result.Envelope, map[string]ed25519.PublicKey{result.Trust.KeyID: key}, time.Now())
	if err != nil {
		t.Fatalf("Go rejected signed ICO-derived PNG: %v", err)
	}
	logo := payload.Entries[0].Logo
	if logo == nil {
		t.Fatal("signed logo lost at consumer")
	}
	decoded, err := base64.StdEncoding.DecodeString(logo.Data)
	if err != nil || !bytes.Equal(decoded, raster.Bytes()) {
		t.Fatal("wire bytes must equal the PNG, not its ICO container")
	}
	digest := sha256.Sum256(raster.Bytes())
	if logo.MIME != "image/png" || logo.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatal("wire MIME and digest must describe the extracted PNG")
	}
	if logo.SourceURL != "https://huggingface.co/favicon.ico" || logo.License != "unknown" {
		t.Fatal("original ICO provenance and rights notice were lost")
	}
}
