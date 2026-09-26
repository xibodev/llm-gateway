package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

// keepSettings republishes the settings a test started with when it ends, so
// fixtures installed through the writers do not leak into later tests.
func keepSettings(t *testing.T) {
	t.Helper()
	original := Get()
	t.Cleanup(func() { Update(func(s *Settings) { *s = *cloneSettings(original) }) })
}

// populatedSettings sets every reference-typed field at every depth, so the
// clone test notices a reference cloneSettings would share.
func populatedSettings() *Settings {
	timeout := 12.5
	s := Defaults()
	s.APIKeys = []string{"fixture-key-a", "fixture-key-b"}
	s.OpenAICodexClientID = "fixture-codex-client"
	s.Providers = map[string]*ProviderConfig{
		"fixture": {Type: "openai_compatible", BaseURL: "https://provider.example.test/v1", Timeout: &timeout},
	}
	s.Endpoints = map[string]*EndpointConfig{
		"smart": {Failover: []EndpointMember{{Provider: "fixture", Model: "model-a"}}},
	}
	s.Policies.Overrides = map[string]ProviderPolicy{"fixture": defaultPolicy()}
	s.Policies.OverrideFields = map[string]map[string]any{
		"fixture": {
			"retry_max_attempts": 3,
			"nested":             map[string]any{"list": []any{map[string]any{"depth": "three"}}},
			"keyed":              map[any]any{1: []any{"one"}},
		},
	}
	s.Savings.PriceCatalog = map[string]map[string]float64{"model-a": {"input": 0.5}}
	return s
}

// assertNoSharedReferences walks original and cloned in step. It fails when
// they share a map, slice or pointer, and when the fixture leaves one unset,
// because an unset reference would hide a field the clone forgot.
func assertNoSharedReferences(t *testing.T, path string, original, cloned reflect.Value) {
	t.Helper()
	if !cloned.IsValid() || cloned.Kind() != original.Kind() {
		t.Errorf("%s is missing from the clone", path)
		return
	}
	switch original.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice:
		if original.IsNil() {
			t.Errorf("fixture leaves %s nil; populate it so the clone is checked there", path)
			return
		}
	}
	switch original.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice:
		if original.Pointer() == cloned.Pointer() {
			t.Errorf("%s is shared between the published value and the clone", path)
		}
	}
	switch original.Kind() {
	case reflect.Pointer, reflect.Interface:
		assertNoSharedReferences(t, path, original.Elem(), cloned.Elem())
	case reflect.Map:
		if original.Len() == 0 || original.Len() != cloned.Len() {
			t.Errorf("%s has %d entries in the fixture and %d in the clone; both must be non-empty and equal", path, original.Len(), cloned.Len())
			return
		}
		entries := original.MapRange()
		for entries.Next() {
			key := entries.Key()
			assertNoSharedReferences(t, fmt.Sprintf("%s[%v]", path, key), entries.Value(), cloned.MapIndex(key))
		}
	case reflect.Slice:
		if original.Len() == 0 || original.Len() != cloned.Len() {
			t.Errorf("%s has %d elements in the fixture and %d in the clone; both must be non-empty and equal", path, original.Len(), cloned.Len())
			return
		}
		for index := range original.Len() {
			assertNoSharedReferences(t, fmt.Sprintf("%s[%d]", path, index), original.Index(index), cloned.Index(index))
		}
	case reflect.Struct:
		for index := range original.NumField() {
			field := original.Type().Field(index).Name
			assertNoSharedReferences(t, path+"."+field, original.Field(index), cloned.Field(index))
		}
	}
}

func TestCloneSettingsSharesNoReferences(t *testing.T) {
	original := populatedSettings()
	cloned := cloneSettings(original)
	assertNoSharedReferences(t, "Settings", reflect.ValueOf(original), reflect.ValueOf(cloned))
	if !reflect.DeepEqual(original, cloned) {
		t.Fatalf("clone differs from the original:\n%+v\n%+v", original, cloned)
	}
}

// deepCopyForTest copies a value by reflection, independently of
// cloneSettings, and keeps nil and empty containers apart so DeepEqual
// compares the copy exactly.
func deepCopyForTest(original reflect.Value) reflect.Value {
	switch original.Kind() {
	case reflect.Pointer:
		if original.IsNil() {
			return reflect.Zero(original.Type())
		}
		copied := reflect.New(original.Type().Elem())
		copied.Elem().Set(deepCopyForTest(original.Elem()))
		return copied
	case reflect.Interface:
		if original.IsNil() {
			return reflect.Zero(original.Type())
		}
		copied := reflect.New(original.Type()).Elem()
		copied.Set(deepCopyForTest(original.Elem()))
		return copied
	case reflect.Map:
		if original.IsNil() {
			return reflect.Zero(original.Type())
		}
		copied := reflect.MakeMapWithSize(original.Type(), original.Len())
		entries := original.MapRange()
		for entries.Next() {
			copied.SetMapIndex(entries.Key(), deepCopyForTest(entries.Value()))
		}
		return copied
	case reflect.Slice:
		if original.IsNil() {
			return reflect.Zero(original.Type())
		}
		copied := reflect.MakeSlice(original.Type(), original.Len(), original.Len())
		for index := range original.Len() {
			copied.Index(index).Set(deepCopyForTest(original.Index(index)))
		}
		return copied
	case reflect.Struct:
		copied := reflect.New(original.Type()).Elem()
		for index := range original.NumField() {
			copied.Field(index).Set(deepCopyForTest(original.Field(index)))
		}
		return copied
	default:
		copied := reflect.New(original.Type()).Elem()
		copied.Set(original)
		return copied
	}
}

// useTempConfig points the state directory and config file at a private
// temporary directory for writers that persist.
func useTempConfig(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", dir)
	t.Setenv("LLMGW_CONFIG", filepath.Join(dir, "config.yaml"))
}

func TestWritersNeverChangePublishedSettings(t *testing.T) {
	keepSettings(t)
	useTempConfig(t)
	Update(func(s *Settings) { *s = *populatedSettings() })
	published := Get()
	want := deepCopyForTest(reflect.ValueOf(published)).Interface().(*Settings)

	// Shaped like the provider upsert in api/admin.go.
	Update(func(s *Settings) {
		next := &ProviderConfig{Type: "openai_compatible"}
		if previous := s.Providers["fixture"]; previous != nil {
			next.Timeout = previous.Timeout
			next.Disabled = previous.Disabled
		}
		s.Providers["upserted"] = next
	})
	// Shaped like the provider enable toggle in api/admin.go.
	Update(func(s *Settings) {
		if provider := s.Providers["fixture"]; provider != nil {
			provider.Disabled = true
			*provider.Timeout = 99
		}
	})
	// Shaped like the Codex client ID update in api/oauth.go.
	Update(func(s *Settings) { s.OpenAICodexClientID = "changed-client" })
	// Shaped like local provider autodetection in detect.go.
	Update(func(s *Settings) {
		configured := map[string]bool{}
		for _, provider := range s.Providers {
			configured[provider.BaseURL] = true
		}
		if !configured["http://127.0.0.1:11434"] {
			s.Providers["ollama"] = &ProviderConfig{Type: "ollama", BaseURL: "http://127.0.0.1:11434"}
		}
	})
	// Every other reference a writer can reach, edited in place.
	Update(func(s *Settings) {
		s.APIKeys[0] = "changed-key"
		s.Endpoints["smart"].Failover[0].Model = "changed-model"
		s.Policies.Overrides["fixture"] = ProviderPolicy{}
		fields := s.Policies.OverrideFields["fixture"]
		fields["retry_max_attempts"] = 9
		fields["nested"].(map[string]any)["list"].([]any)[0].(map[string]any)["depth"] = "changed"
		fields["keyed"].(map[any]any)[1].([]any)[0] = "changed"
		s.Savings.PriceCatalog["model-a"]["input"] = 9
	})
	restore, err := UpdateAndSave(func(s *Settings) error {
		delete(s.Providers, "fixture")
		s.Endpoints["smart"].Failover = append(s.Endpoints["smart"].Failover, EndpointMember{Provider: "upserted", Model: "model-b"})
		return nil
	})
	if err != nil {
		t.Fatalf("update and save: %v", err)
	}
	if err := restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if added, err := AddProviderIfMissing("added", &ProviderConfig{Type: "echo"}); err != nil || !added {
		t.Fatalf("added=%v err=%v", added, err)
	}
	if err := Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	Load()

	if Get() == published {
		t.Fatal("writers did not publish a new value")
	}
	if !reflect.DeepEqual(published, want) {
		t.Fatalf("a writer changed settings published before it ran:\n got %+v\nwant %+v", published, want)
	}
}

func TestEveryPublicationAdvancesTheGeneration(t *testing.T) {
	keepSettings(t)
	useTempConfig(t)
	_, last := Snapshot()
	check := func(step string, advanced bool) {
		t.Helper()
		settings, generation := Snapshot()
		sourced, sourcedGeneration := Source{}.Snapshot()
		if settings != Get() || generation != Generation() || sourced != settings || sourcedGeneration != generation {
			t.Fatalf("%s: snapshot pairs %p at %d with %p at %d", step, settings, generation, Get(), Generation())
		}
		switch {
		case advanced && generation <= last:
			t.Fatalf("%s left the generation at %d after %d", step, generation, last)
		case !advanced && generation != last:
			t.Fatalf("%s moved the generation from %d to %d without publishing", step, last, generation)
		}
		last = generation
	}

	Update(func(s *Settings) { s.GatewayPreamble = "updated" })
	check("Update", true)
	if Get().GatewayPreamble != "updated" {
		t.Fatalf("snapshot after Update holds %q", Get().GatewayPreamble)
	}
	restore, err := UpdateAndSave(func(s *Settings) error {
		s.Providers["saved"] = &ProviderConfig{Type: "echo"}
		return nil
	})
	if err != nil {
		t.Fatalf("update and save: %v", err)
	}
	check("UpdateAndSave", true)
	if err := restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	check("restore", true)
	if Get().Providers["saved"] != nil {
		t.Fatal("restore did not republish the previous settings")
	}
	if _, err := UpdateAndSave(func(*Settings) error { return errors.New("rejected") }); err == nil {
		t.Fatal("rejected update reported success")
	}
	check("rejected UpdateAndSave", false)
	if added, err := AddProviderIfMissing("added", &ProviderConfig{Type: "echo"}); err != nil || !added {
		t.Fatalf("added=%v err=%v", added, err)
	}
	check("AddProviderIfMissing", true)
	if added, err := AddProviderIfMissing("added", &ProviderConfig{Type: "echo"}); err != nil || added {
		t.Fatalf("duplicate added=%v err=%v", added, err)
	}
	check("duplicate AddProviderIfMissing", false)
	if err := Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	check("Save", false)
	Load()
	check("Load", true)
	Load()
	check("repeated Load", true)
}

func TestRestoreDoesNotClobberANewerPublication(t *testing.T) {
	keepSettings(t)
	useTempConfig(t)
	Update(func(s *Settings) { s.Providers = map[string]*ProviderConfig{"kept": {Type: "echo"}} })
	previous := deepCopyForTest(reflect.ValueOf(Get())).Interface().(*Settings)

	stage := func() func() error {
		t.Helper()
		restore, err := UpdateAndSave(func(s *Settings) error {
			s.Providers["staged"] = &ProviderConfig{Type: "echo"}
			return nil
		})
		if err != nil {
			t.Fatalf("update and save: %v", err)
		}
		return restore
	}

	restore := stage()
	if err := restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !reflect.DeepEqual(Get(), previous) {
		t.Fatalf("restore published %+v, want %+v", Get(), previous)
	}
	if err := restore(); !errors.Is(err, ErrRestoreSuperseded) {
		t.Fatalf("second restore err=%v, want ErrRestoreSuperseded", err)
	}

	restore = stage()
	Update(func(s *Settings) { s.OpenAICodexClientID = "newer" })
	newer, generation := Snapshot()
	onDisk, err := os.ReadFile(ConfigFilePath())
	if err != nil {
		t.Fatal(err)
	}
	if err := restore(); !errors.Is(err, ErrRestoreSuperseded) {
		t.Fatalf("stale restore err=%v, want ErrRestoreSuperseded", err)
	}
	if current, currentGeneration := Snapshot(); current != newer || currentGeneration != generation {
		t.Fatalf("stale restore replaced generation %d with %d", generation, currentGeneration)
	}
	if after, err := os.ReadFile(ConfigFilePath()); err != nil || string(after) != string(onDisk) {
		t.Fatalf("stale restore rewrote the config file: err=%v\n%s", err, after)
	}
}

// Readers range over published maps without a lock while writers insert and
// delete providers. With in-place updates this aborted the process with
// "concurrent map iteration and map write"; it must now run clean, and every
// snapshot must pair settings with the generation they were published under.
func TestReadersSurviveConcurrentWriters(t *testing.T) {
	keepSettings(t)
	useTempConfig(t)
	// stamp runs inside a writer's critical section, where the copy is about
	// to be published under the generation after the current one.
	stamp := func(s *Settings) { s.GatewayPreamble = strconv.FormatUint(Generation()+1, 10) }
	Update(func(s *Settings) {
		s.Providers = map[string]*ProviderConfig{"stable": {Type: "echo"}}
		stamp(s)
	})

	const readers, writers, rounds = 8, 4, 2000
	var (
		writersDone atomic.Bool
		faults      atomic.Int64
		readerGroup sync.WaitGroup
		writerGroup sync.WaitGroup
	)
	for range readers {
		readerGroup.Go(func() {
			var last uint64
			for !writersDone.Load() {
				settings, generation := Snapshot()
				if generation < last || settings.GatewayPreamble != strconv.FormatUint(generation, 10) {
					faults.Add(1)
				}
				last = generation
				for id, provider := range settings.Providers {
					if id == "" || provider == nil || provider.Type != "echo" {
						faults.Add(1)
					}
				}
				_ = Get().OpenAICodexClientID
				if provider, ok := Provider("stable"); !ok || provider.Type != "echo" {
					faults.Add(1)
				}
			}
		})
	}
	for writer := range writers {
		writerGroup.Go(func() {
			for round := range rounds {
				id := fmt.Sprintf("writer-%d-%d", writer, round%8)
				Update(func(s *Settings) {
					if _, ok := s.Providers[id]; ok {
						delete(s.Providers, id)
					} else {
						s.Providers[id] = &ProviderConfig{Type: "echo"}
					}
					s.Providers["stable"].Disabled = !s.Providers["stable"].Disabled
					s.OpenAICodexClientID = id
					stamp(s)
				})
			}
		})
	}
	writerGroup.Go(func() {
		for round := range 10 {
			_, err := UpdateAndSave(func(s *Settings) error {
				s.Providers[fmt.Sprintf("persisted-%d", round)] = &ProviderConfig{Type: "echo"}
				stamp(s)
				return nil
			})
			if err != nil {
				faults.Add(1)
			}
		}
	})
	writerGroup.Wait()
	writersDone.Store(true)
	readerGroup.Wait()
	if count := faults.Load(); count > 0 {
		t.Fatalf("%d reads saw an inconsistent snapshot or a failed save", count)
	}
}
