package roster

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestSignedMetadataSurvivesSnapshotAndCache(t *testing.T) {
	pub, key, p := fixture(t)
	source := Source{Repo: "owner/directory", Commit: "test-commit", URL: "https://example.com/directory", Status: "ok", License: "MIT", LicenseURL: "https://example.com/directory/LICENSE", Notice: "Copyright Example contributors\nRetain this notice."}
	p.Sources = []Source{source}
	p.Entries[0].Sources = []Source{source}
	p.Entries[0].DocsURL = "https://example.com/docs/api"
	body := signed(t, key, p)
	var calls atomic.Int32
	o := Options{URL: "https://feed.example.com/roster.json", Keys: map[string]ed25519.PublicKey{"staging": pub}, StateDir: t.TempDir(), Client: clientFor(&body, &calls)}
	s := New(o)
	for _, got := range []Snapshot{s.Refresh(context.Background()), New(o).Snapshot()} {
		if got.Error != "" || !reflect.DeepEqual(got.Sources, p.Sources) || !reflect.DeepEqual(got.Entries, p.Entries) {
			t.Fatalf("signed metadata lost: %+v", got)
		}
		b, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		var response struct {
			Entries []struct {
				DocsURL string              `json:"docs_url"`
				Sources []map[string]string `json:"sources"`
			} `json:"entries"`
			Sources []map[string]string `json:"sources"`
		}
		if err := json.Unmarshal(b, &response); err != nil {
			t.Fatal(err)
		}
		if response.Entries[0].DocsURL != p.Entries[0].DocsURL {
			t.Fatal("docs_url missing in wire response")
		}
		for _, wire := range []map[string]string{response.Sources[0], response.Entries[0].Sources[0]} {
			if wire["license"] != source.License || wire["license_url"] != source.LicenseURL || wire["notice"] != source.Notice {
				t.Fatalf("source notice missing in wire response: %v", wire)
			}
		}
	}
	for _, rawURL := range []string{"http://example.com/docs", "https://user:secret@example.com/docs", "https://example.com/docs?token=secret", "https://127.0.0.1/docs"} {
		for _, field := range []string{"docs_url", "license_url", "entry_license_url"} {
			t.Run(field+"/"+rawURL, func(t *testing.T) {
				_, _, invalid := fixture(t)
				switch field {
				case "docs_url":
					invalid.Entries[0].DocsURL = rawURL
				case "license_url":
					invalid.Sources = []Source{{LicenseURL: rawURL}}
				case "entry_license_url":
					invalid.Entries[0].Sources = []Source{{LicenseURL: rawURL}}
				}
				if _, _, err := verify(signed(t, key, invalid), o.Keys, time.Now()); err == nil {
					t.Fatal("unsafe metadata URL accepted")
				}
			})
		}
	}
}

func TestFreshSignedPublicationRetainsSourceStaleness(t *testing.T) {
	for _, mode := range []string{"all", "partial", "entry"} {
		t.Run(mode, func(t *testing.T) {
			pub, key, p := fixture(t)
			p.Sources = []Source{{Repo: "owner/directory", Status: "ok"}}
			body := signed(t, key, p)
			var calls atomic.Int32
			o := Options{URL: "https://feed.example.com/roster.json", Keys: map[string]ed25519.PublicKey{"staging": pub}, StateDir: t.TempDir(), Client: clientFor(&body, &calls)}
			s := New(o)
			now := time.Now().Add(-time.Hour)
			s.clock = func() time.Time { return now }
			if got := s.Refresh(context.Background()); got.Stale || got.Error != "" {
				t.Fatalf("initial healthy feed: %+v", got)
			}
			p.Revision++
			now = now.Add(refreshCooldown + time.Second)
			p.PublishedAt = now.UTC().Format(time.RFC3339)
			switch mode {
			case "all":
				p.Sources[0].Status = "stale"
			case "partial":
				p.Sources = append(p.Sources, Source{Repo: "owner/other", Status: "stale"})
			case "entry":
				p.Entries[0].Sources = []Source{{Repo: "owner/directory", Status: "stale"}}
			}
			body = signed(t, key, p)
			for _, got := range []Snapshot{s.Refresh(context.Background()), New(o).Snapshot()} {
				if !got.Stale || got.Error != "" || got.Revision != p.Revision || got.PublishedAt != p.PublishedAt {
					t.Fatalf("fresh publication hid stale collection: %+v", got)
				}
			}
			p.Revision++
			p.Sources = []Source{{Repo: "owner/directory", Status: "ok"}}
			p.Entries[0].Sources = nil
			now = now.Add(refreshCooldown + time.Second)
			p.PublishedAt = now.UTC().Format(time.RFC3339)
			body = signed(t, key, p)
			if got := s.Refresh(context.Background()); got.Stale || got.Error != "" {
				t.Fatalf("healthy sources did not clear stale: %+v", got)
			}
		})
	}
}
