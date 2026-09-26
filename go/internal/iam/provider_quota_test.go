package iam

import (
	"errors"
	"testing"
	"time"
)

func TestProviderQuotaSnapshotsReplaceAndFilter(t *testing.T) {
	setupConnectionTest(t)
	firstOwner, err := CreatePrincipal("human", "authentik:quota-first", "", "Quota First")
	if err != nil {
		t.Fatal(err)
	}
	secondOwner, err := CreatePrincipal("human", "authentik:quota-second", "", "Quota Second")
	if err != nil {
		t.Fatal(err)
	}
	first, err := PutProviderConnection(ProviderConnectionCreate{
		PrincipalID: firstOwner.ID, ProviderID: "copilot", Name: "personal",
		Kind: "api_key", Secret: "first-secret", Source: ConnectionSourceUser,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := PutProviderConnection(ProviderConnectionCreate{
		PrincipalID: secondOwner.ID, ProviderID: "copilot", Name: "work",
		Kind: "api_key", Secret: "second-secret", Source: ConnectionSourceUser,
	})
	if err != nil {
		t.Fatal(err)
	}
	firstRevision := providerQuotaRevision(t, first.ID)
	secondRevision := providerQuotaRevision(t, second.ID)

	limit := 100.0
	used := 25.0
	remaining := 75.0
	if err := ReplaceProviderQuotaSnapshots(first.ID, firstRevision, []ProviderQuotaSnapshot{
		{
			Metric: "premium", Unit: "percent", Window: "month", Label: "Premium",
			LimitValue: &limit, UsedValue: &used, RemainingValue: &remaining,
			ResetAt: 2800, Source: "copilot_api", Confidence: "verified",
			RefreshedAt: 2000, ExpiresAt: 2600, Metadata: map[string]any{"tier": "enterprise"},
		},
		{
			Metric: "chat", Unit: "requests", Window: "day",
			RemainingValue: &remaining, Source: "copilot_api", Confidence: "verified",
			RefreshedAt: 2000, ExpiresAt: 2600,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceProviderQuotaSnapshots(second.ID, secondRevision, []ProviderQuotaSnapshot{
		{
			Metric: "premium", Unit: "percent", Window: "month",
			RemainingValue: &remaining, Source: "copilot_api", Confidence: "verified",
			RefreshedAt: 2100, ExpiresAt: 2700,
		},
	}); err != nil {
		t.Fatal(err)
	}

	all, err := ListProviderQuotaSnapshots(ProviderQuotaFilter{ProviderID: "copilot"})
	if err != nil || len(all) != 3 {
		t.Fatalf("all snapshots=%+v err=%v", all, err)
	}
	ownerOnly, err := ListProviderQuotaSnapshots(ProviderQuotaFilter{
		ProviderID: "copilot", PrincipalID: firstOwner.ID,
	})
	if err != nil || len(ownerOnly) != 2 {
		t.Fatalf("owner snapshots=%+v err=%v", ownerOnly, err)
	}
	fresh, err := ListProviderQuotaSnapshots(ProviderQuotaFilter{
		ProviderID: "copilot", FreshOnly: true, Now: 2650,
	})
	if err != nil || len(fresh) != 1 || fresh[0].ConnectionID != second.ID {
		t.Fatalf("fresh snapshots=%+v err=%v", fresh, err)
	}
	state, found, err := ProviderAccountStateByConnection(first.ID)
	if err != nil || !found || state.LastQuotaRefresh != 2000 {
		t.Fatalf("account state=%+v found=%v err=%v", state, found, err)
	}

	replacement := 45.0
	if err := ReplaceProviderQuotaSnapshots(first.ID, firstRevision, []ProviderQuotaSnapshot{{
		Metric: "premium", Unit: "percent", Window: "month",
		RemainingValue: &replacement, Source: "copilot_api", Confidence: "verified",
		RefreshedAt: 2200,
	}}); err != nil {
		t.Fatal(err)
	}
	firstOnly, err := ListProviderQuotaSnapshots(ProviderQuotaFilter{ConnectionID: first.ID})
	if err != nil || len(firstOnly) != 1 || *firstOnly[0].RemainingValue != replacement {
		t.Fatalf("replacement=%+v err=%v", firstOnly, err)
	}

	negative := -1.0
	err = ReplaceProviderQuotaSnapshots(first.ID, firstRevision, []ProviderQuotaSnapshot{{
		Metric: "premium", Unit: "percent", Window: "month",
		RemainingValue: &negative, Source: "copilot_api", Confidence: "verified",
		RefreshedAt: 2300,
	}})
	if err == nil {
		t.Fatal("negative quota should be rejected")
	}
	err = ReplaceProviderQuotaSnapshots(first.ID, firstRevision, []ProviderQuotaSnapshot{{
		Metric: "premium", Unit: "percent", Window: "month",
		Source: "copilot_api", Confidence: "verified",
		RefreshedAt: time.Now().Add(time.Hour).Unix(),
	}})
	if err == nil {
		t.Fatal("future quota refresh timestamp should be rejected")
	}
	unchanged, err := ListProviderQuotaSnapshots(ProviderQuotaFilter{ConnectionID: first.ID})
	if err != nil || len(unchanged) != 1 || *unchanged[0].RemainingValue != replacement {
		t.Fatalf("invalid replacement changed stored snapshots: %+v err=%v", unchanged, err)
	}

	err = ReplaceProviderQuotaSnapshots(first.ID, firstRevision, []ProviderQuotaSnapshot{{
		Metric: "premium", Unit: "percent", Window: "month",
		Source: "copilot_api", Confidence: "verified", RefreshedAt: 2300,
		Metadata: map[string]any{"access_token": "must-not-store"},
	}})
	if err == nil {
		t.Fatal("credential-shaped quota metadata should be rejected")
	}
	for key, value := range map[string]string{
		"apiKey": "plain-value",
		"note":   "sk-secret-looking-value",
	} {
		err = ReplaceProviderQuotaSnapshots(first.ID, firstRevision, []ProviderQuotaSnapshot{{
			Metric: "premium", Unit: "percent", Window: "month",
			Source: "copilot_api", Confidence: "verified", RefreshedAt: 2300,
			Metadata: map[string]any{key: value},
		}})
		if err == nil {
			t.Fatalf("credential-shaped metadata %q should be rejected", key)
		}
	}
	if err := ReplaceProviderQuotaSnapshots("missing", 1, nil); !errors.Is(
		err, ErrProviderConnectionNotFound,
	) {
		t.Fatalf("missing connection error=%v", err)
	}
}

func TestProviderQuotaClearsWhenCredentialsChange(t *testing.T) {
	setupConnectionTest(t)
	human, err := CreatePrincipal("human", "authentik:quota-rotation", "", "Quota Rotation")
	if err != nil {
		t.Fatal(err)
	}
	connection, err := PutProviderConnection(ProviderConnectionCreate{
		PrincipalID: human.ID, ProviderID: "gemini", Name: "personal",
		Kind: "api_key", Secret: "first-secret", Source: ConnectionSourceUser,
	})
	if err != nil {
		t.Fatal(err)
	}
	remaining := 50.0
	storeQuota := func() {
		t.Helper()
		if err := ReplaceProviderQuotaSnapshots(
			connection.ID, providerQuotaRevision(t, connection.ID), []ProviderQuotaSnapshot{{
				Metric: "requests", Unit: "percent", Window: "day",
				RemainingValue: &remaining, Source: "fixture", Confidence: "verified",
				RefreshedAt: time.Now().Unix(),
			}}); err != nil {
			t.Fatal(err)
		}
	}
	assertCleared := func() {
		t.Helper()
		snapshots, err := ListProviderQuotaSnapshots(ProviderQuotaFilter{ConnectionID: connection.ID})
		if err != nil || len(snapshots) != 0 {
			t.Fatalf("quota snapshots=%+v err=%v", snapshots, err)
		}
		state, found, err := ProviderAccountStateByConnection(connection.ID)
		if err != nil || !found || state.LastQuotaRefresh != 0 {
			t.Fatalf("account state=%+v found=%v err=%v", state, found, err)
		}
	}

	storeQuota()
	oldRevision := providerQuotaRevision(t, connection.ID)
	connection, err = PutProviderConnection(ProviderConnectionCreate{
		PrincipalID: human.ID, ProviderID: "gemini", Name: "personal",
		Kind: "api_key", Secret: "rotated-secret", Source: ConnectionSourceUser,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertCleared()
	err = ReplaceProviderQuotaSnapshots(connection.ID, oldRevision, []ProviderQuotaSnapshot{{
		Metric: "requests", Unit: "percent", Window: "day",
		RemainingValue: &remaining, Source: "stale-refresh", Confidence: "verified",
		RefreshedAt: time.Now().Unix(),
	}})
	if !errors.Is(err, ErrProviderQuotaCredentialChanged) {
		t.Fatalf("stale credential revision error=%v", err)
	}

	storeQuota()
	if err := RevokeProviderConnection(human.ID, connection.ID); err != nil {
		t.Fatal(err)
	}
	assertCleared()
}

func TestProviderQuotaClearsWhenOAuthConnectionRotates(t *testing.T) {
	setupConnectionTest(t)
	human, err := CreatePrincipal("human", "authentik:quota-oauth", "", "Quota OAuth")
	if err != nil {
		t.Fatal(err)
	}
	connection, err := PutOAuthProviderConnection(OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "copilot", Kind: "github_oauth",
		AccessToken: "first-access",
	})
	if err != nil {
		t.Fatal(err)
	}
	remaining := 90.0
	if err := ReplaceProviderQuotaSnapshots(
		connection.ID, providerQuotaRevision(t, connection.ID), []ProviderQuotaSnapshot{{
			Metric: "premium", Unit: "percent", Window: "month",
			RemainingValue: &remaining, Source: "fixture", Confidence: "verified",
			RefreshedAt: time.Now().Unix(),
		}}); err != nil {
		t.Fatal(err)
	}
	envelope, current, ok, err := OAuthProviderConnectionSecret(human.ID, "copilot", "")
	if err != nil || !ok {
		t.Fatalf("OAuth connection ok=%v err=%v", ok, err)
	}
	if _, err := ReplaceOAuthProviderConnectionIfCurrent(current, envelope, OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "copilot", Name: current.Name,
		Kind: current.Kind, Source: current.Source, MakeDefault: current.IsDefault,
		AccessToken: "rotated-access",
	}); err != nil {
		t.Fatal(err)
	}
	snapshots, err := ListProviderQuotaSnapshots(ProviderQuotaFilter{ConnectionID: connection.ID})
	if err != nil || len(snapshots) != 0 {
		t.Fatalf("OAuth rotation retained quota snapshots=%+v err=%v", snapshots, err)
	}
}

func TestProviderQuotaFreshFilterRejectsFutureRows(t *testing.T) {
	setupConnectionTest(t)
	human, err := CreatePrincipal("human", "authentik:quota-future", "", "Quota Future")
	if err != nil {
		t.Fatal(err)
	}
	connection, err := PutProviderConnection(ProviderConnectionCreate{
		PrincipalID: human.ID, ProviderID: "gemini", Kind: "api_key",
		Secret: "fixture-secret", Source: ConnectionSourceUser,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	remaining := 50.0
	if err := ReplaceProviderQuotaSnapshots(
		connection.ID, providerQuotaRevision(t, connection.ID), []ProviderQuotaSnapshot{{
			Metric: "requests", Unit: "percent", Window: "day",
			RemainingValue: &remaining, Source: "fixture", Confidence: "verified",
			RefreshedAt: now,
		}},
	); err != nil {
		t.Fatal(err)
	}
	db, err := DB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		"UPDATE provider_quota_snapshots SET refreshed_at=? WHERE connection_id=?",
		now+int64(time.Hour/time.Second), connection.ID,
	); err != nil {
		t.Fatal(err)
	}
	fresh, err := ListProviderQuotaSnapshots(ProviderQuotaFilter{
		ConnectionID: connection.ID, FreshOnly: true, Now: now,
	})
	if err != nil || len(fresh) != 0 {
		t.Fatalf("future-dated fresh snapshots=%+v err=%v", fresh, err)
	}
}

func providerQuotaRevision(t *testing.T, connectionID string) int64 {
	t.Helper()
	state, found, err := ProviderAccountStateByConnection(connectionID)
	if err != nil || !found {
		t.Fatalf("provider account state=%+v found=%v err=%v", state, found, err)
	}
	return state.CredentialRevision
}
