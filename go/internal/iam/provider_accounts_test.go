package iam

import (
	"errors"
	"testing"
	"time"
)

func TestProviderAccountStateLifecycle(t *testing.T) {
	setupConnectionTest(t)
	human, err := CreatePrincipal("human", "authentik:provider-state", "", "Provider State")
	if err != nil {
		t.Fatal(err)
	}
	connection, err := PutProviderConnection(ProviderConnectionCreate{
		PrincipalID: human.ID, ProviderID: "gemini", Name: "personal",
		Kind: "api_key", Secret: "fixture-secret", Source: ConnectionSourceUser,
	})
	if err != nil {
		t.Fatal(err)
	}
	state, found, err := ProviderAccountStateByConnection(connection.ID)
	if err != nil || !found {
		t.Fatalf("default state=%+v found=%v err=%v", state, found, err)
	}
	if state.Priority != DefaultProviderAccountPriority || state.HealthStatus != "unknown" {
		t.Fatalf("default state=%+v", state)
	}

	priority := 10
	health := "degraded"
	expiresAt := int64(5000)
	label := "Owner account"
	tier := "Pro"
	proxyRef := "proxy-one"
	quotaRefresh := int64(1200)
	state, err = UpdateProviderAccountState(connection.ID, ProviderAccountStateUpdate{
		Priority: &priority, HealthStatus: &health, TokenExpiresAt: &expiresAt,
		AccountLabel: &label, AccountTier: &tier, ProxyRef: &proxyRef,
		LastQuotaRefresh: &quotaRefresh,
	})
	if err != nil {
		t.Fatal(err)
	}
	if state.Priority != priority || state.HealthStatus != health ||
		state.TokenExpiresAt != expiresAt || state.AccountLabel != label ||
		state.AccountTier != tier || state.ProxyRef != proxyRef ||
		state.LastQuotaRefresh != quotaRefresh {
		t.Fatalf("updated state=%+v", state)
	}

	failedAt := time.Unix(1300, 0)
	state, err = RecordProviderAccountFailure(
		connection.ID, "rate_limited", time.Unix(1600, 0), failedAt,
	)
	if err != nil {
		t.Fatal(err)
	}
	if state.HealthStatus != "cooldown" || state.ConsecutiveFailures != 1 ||
		state.CooldownUntil != 1600 || state.LastFailureAt != 1300 ||
		state.LastFailureCode != "rate_limited" {
		t.Fatalf("failed state=%+v", state)
	}
	state, err = RecordProviderAccountSuccess(connection.ID, time.Unix(1200, 0))
	if err != nil {
		t.Fatal(err)
	}
	if state.HealthStatus != "cooldown" || state.LastFailureAt != 1300 {
		t.Fatalf("stale success overwrote newer failure: %+v", state)
	}

	state, err = RecordProviderAccountSuccess(connection.ID, time.Unix(1700, 0))
	if err != nil {
		t.Fatal(err)
	}
	if state.HealthStatus != "healthy" || state.ConsecutiveFailures != 0 ||
		state.CooldownUntil != 0 || state.LastSuccessAt != 1700 ||
		state.LastFailureCode != "" {
		t.Fatalf("healthy state=%+v", state)
	}
	state, err = RecordProviderAccountFailure(
		connection.ID, "stale_failure", time.Unix(1900, 0), time.Unix(1600, 0),
	)
	if err != nil {
		t.Fatal(err)
	}
	if state.HealthStatus != "healthy" || state.LastSuccessAt != 1700 ||
		state.LastFailureCode != "" {
		t.Fatalf("stale failure overwrote newer success: %+v", state)
	}
	state, err = RecordProviderAccountFailure(
		connection.ID, "same_second_failure",
		time.Unix(1801, 0), time.Unix(1800, 100),
	)
	if err != nil {
		t.Fatal(err)
	}
	state, err = RecordProviderAccountSuccess(connection.ID, time.Unix(1800, 200))
	if err != nil {
		t.Fatal(err)
	}
	if state.HealthStatus != "healthy" || state.LastSuccessAt != 1800 {
		t.Fatalf("later same-second success was discarded: %+v", state)
	}
	state, err = RecordProviderAccountFailure(
		connection.ID, "older_same_second_failure",
		time.Unix(1802, 0), time.Unix(1800, 150),
	)
	if err != nil {
		t.Fatal(err)
	}
	if state.HealthStatus != "healthy" || state.LastFailureCode != "" {
		t.Fatalf("older same-second failure overwrote success: %+v", state)
	}

	invalid := "not-a-health-state"
	if _, err := UpdateProviderAccountState(connection.ID, ProviderAccountStateUpdate{
		HealthStatus: &invalid,
	}); err == nil {
		t.Fatal("invalid health state should be rejected")
	}
	if _, err := UpdateProviderAccountState("missing", ProviderAccountStateUpdate{}); !errors.Is(
		err, ErrProviderConnectionNotFound,
	) {
		t.Fatalf("missing connection error=%v", err)
	}
}

func TestOAuthConnectionPersistsProviderAccountMetadata(t *testing.T) {
	setupConnectionTest(t)
	human, err := CreatePrincipal("human", "authentik:oauth-state", "", "OAuth State")
	if err != nil {
		t.Fatal(err)
	}
	connection, err := PutOAuthProviderConnection(OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "copilot", Kind: "github_oauth",
		AccessToken: "access-one", ExpiresAt: 2000, AccountLabel: "First account",
	})
	if err != nil {
		t.Fatal(err)
	}
	state, found, err := ProviderAccountStateByConnection(connection.ID)
	if err != nil || !found || state.HealthStatus != "unknown" ||
		state.TokenExpiresAt != 2000 || state.AccountLabel != "First account" {
		t.Fatalf("initial OAuth state=%+v found=%v err=%v", state, found, err)
	}

	envelope, current, ok, err := OAuthProviderConnectionSecret(human.ID, "copilot", "")
	if err != nil || !ok {
		t.Fatalf("load OAuth connection ok=%v err=%v", ok, err)
	}
	if _, err := RecordProviderAccountFailure(
		connection.ID, "temporary", time.Now().Add(time.Hour), time.Now(),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := ReplaceOAuthProviderConnectionIfCurrent(current, envelope, OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "copilot", Name: current.Name,
		Kind: current.Kind, Source: current.Source, MakeDefault: current.IsDefault,
		AccessToken: "access-two", ExpiresAt: 0, AccountLabel: "",
	}); err != nil {
		t.Fatal(err)
	}
	state, found, err = ProviderAccountStateByConnection(connection.ID)
	if err != nil || !found || state.TokenExpiresAt != 0 ||
		state.AccountLabel != "" || state.HealthStatus != "unknown" ||
		state.ConsecutiveFailures != 0 || state.CooldownUntil != 0 ||
		state.LastFailureCode != "" || state.LastSuccessAt != 0 {
		t.Fatalf("rotated OAuth state=%+v found=%v err=%v", state, found, err)
	}
}

func TestAPIKeyRotationResetsProviderAccountHealth(t *testing.T) {
	setupConnectionTest(t)
	human, err := CreatePrincipal("human", "authentik:key-rotation", "", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	connection, err := PutProviderConnection(ProviderConnectionCreate{
		PrincipalID: human.ID, ProviderID: "fixture", Name: "personal",
		Kind: "api_key", Secret: "first", Source: ConnectionSourceUser,
		MakeDefault: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RecordProviderAccountSuccess(connection.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	before, found, err := ProviderAccountStateByConnection(connection.ID)
	if err != nil || !found || before.HealthStatus != "healthy" {
		t.Fatalf("before=%+v found=%v err=%v", before, found, err)
	}
	if _, err := PutProviderConnection(ProviderConnectionCreate{
		PrincipalID: human.ID, ProviderID: "fixture", Name: "personal",
		Kind: "api_key", Secret: "second", Source: ConnectionSourceUser,
		MakeDefault: true,
	}); err != nil {
		t.Fatal(err)
	}
	after, found, err := ProviderAccountStateByConnection(connection.ID)
	if err != nil || !found || after.HealthStatus != "unknown" ||
		after.LastSuccessAt != 0 || after.CredentialRevision <= before.CredentialRevision {
		t.Fatalf("after=%+v found=%v err=%v", after, found, err)
	}
}

func TestProviderAccountObservationRejectsStaleCredentialRevision(t *testing.T) {
	setupConnectionTest(t)
	human, err := CreatePrincipal("human", "authentik:revision-owner", "", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	connection, err := PutOAuthProviderConnection(OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "copilot", Kind: "github_oauth",
		AccessToken: "access-one",
	})
	if err != nil {
		t.Fatal(err)
	}
	observation, found, err := ActiveProviderAccountObservation(human.ID, "copilot")
	if err != nil || !found || observation.ConnectionID != connection.ID {
		t.Fatalf("observation=%+v found=%v err=%v", observation, found, err)
	}
	envelope, current, ok, err := OAuthProviderConnectionSecret(human.ID, "copilot", "")
	if err != nil || !ok {
		t.Fatalf("load connection ok=%v err=%v", ok, err)
	}
	if _, err := ReplaceOAuthProviderConnectionIfCurrent(current, envelope, OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "copilot", Name: current.Name,
		Kind: current.Kind, Source: current.Source, MakeDefault: current.IsDefault,
		AccessToken: "access-two",
	}); err != nil {
		t.Fatal(err)
	}
	applied, err := RecordProviderAccountFailureIfCurrent(
		observation, "stale_failure", time.Now(),
	)
	if err != nil || applied {
		t.Fatalf("stale result applied=%v err=%v", applied, err)
	}
	state, found, err := ProviderAccountStateByConnection(connection.ID)
	if err != nil || !found || state.HealthStatus != "unknown" ||
		state.LastFailureCode != "" {
		t.Fatalf("rotated state=%+v found=%v err=%v", state, found, err)
	}
}

func TestBackfillProviderAccountOAuthMetadata(t *testing.T) {
	setupConnectionTest(t)
	human, err := CreatePrincipal("human", "authentik:oauth-backfill", "", "OAuth Backfill")
	if err != nil {
		t.Fatal(err)
	}
	connection, err := PutOAuthProviderConnection(OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "copilot", Kind: "github_oauth",
		AccessToken: "access-one", ExpiresAt: 4000, AccountLabel: "Backfilled account",
	})
	if err != nil {
		t.Fatal(err)
	}
	db, err := DB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DELETE FROM provider_account_state WHERE connection_id=?", connection.ID); err != nil {
		t.Fatal(err)
	}
	if err := SetPrincipalStatus(human.ID, "disabled"); err != nil {
		t.Fatal(err)
	}
	updated, err := BackfillProviderAccountOAuthMetadata()
	if err != nil || updated != 1 {
		t.Fatalf("backfill updated=%d err=%v", updated, err)
	}
	state, found, err := ProviderAccountStateByConnection(connection.ID)
	if err != nil || !found || state.TokenExpiresAt != 4000 ||
		state.AccountLabel != "Backfilled account" {
		t.Fatalf("backfilled state=%+v found=%v err=%v", state, found, err)
	}
}
