/*
 * Copyright 2026 InfAI (CC SES)
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package profiler

import (
	"errors"
	"testing"
	"time"
)

func testRef() SeriesRef {
	return SeriesRef{
		DeviceID:     "urn:infai:ses:device:1",
		ServiceID:    "urn:infai:ses:service:1",
		VariablePath: "value.power",
	}
}

func testWindows() (Window, Window) {
	analysis := Window{From: fixtureStart, To: fixtureStart.Add(90 * 24 * time.Hour)}
	raw := Window{From: analysis.To.Add(-14 * 24 * time.Hour), To: analysis.To}
	return analysis, raw
}

func storedProfile(t *testing.T, store Store, detectorVersion string, unit string) SeriesProfile {
	t.Helper()
	analysis, raw := testWindows()
	key := CacheKey(testRef(), analysis, raw, detectorVersion)
	profile := SeriesProfile{
		ProfileID:       key,
		CacheKey:        key,
		Tier:            TierFull,
		SeriesRef:       testRef(),
		DetectorVersion: detectorVersion,
		AnalysisWindow:  analysis,
		RawWindow:       RawWindow{Window: raw, Source: WindowDefault},
		ValueSemantics:  ValueSemantics{Unit: unit, UnitSource: UnitInferred},
	}
	stored, _, err := store.Put(profile, nil)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	return stored
}

// D25: detector_version belongs in the cache key, or improving a detector leaves
// stale profiles in the LLM's context with nothing to notice them by.
func TestTheCacheKeyChangesWithTheDetectorVersion(t *testing.T) {
	analysis, raw := testWindows()

	first := CacheKey(testRef(), analysis, raw, "1.0.0")
	second := CacheKey(testRef(), analysis, raw, "1.1.0")
	if first == second {
		t.Error("the cache key ignores the detector version")
	}
}

func TestTheCacheKeyChangesWithEitherWindow(t *testing.T) {
	analysis, raw := testWindows()
	base := CacheKey(testRef(), analysis, raw, DetectorVersion)

	widerAnalysis := Window{From: analysis.From.Add(-24 * time.Hour), To: analysis.To}
	if CacheKey(testRef(), widerAnalysis, raw, DetectorVersion) == base {
		t.Error("the cache key ignores the analysis window")
	}
	widerRaw := Window{From: raw.From.Add(-24 * time.Hour), To: raw.To}
	if CacheKey(testRef(), analysis, widerRaw, DetectorVersion) == base {
		t.Error("the cache key ignores the raw window")
	}
}

// D21: profiles are immutable. A recomputation must not quietly alter something a
// developer has already read and confirmed against.
func TestStoringTheSameProfileTwiceKeepsTheFirst(t *testing.T) {
	store := NewMemoryStore()
	first := storedProfile(t, store, DetectorVersion, "W")

	analysis, raw := testWindows()
	replacement := first
	replacement.ValueSemantics.Unit = "kW"
	stored, created, err := store.Put(replacement, nil)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if created {
		t.Error("the store reported a new profile for an existing cache key")
	}
	if stored.ValueSemantics.Unit != "W" {
		t.Errorf("unit = %q, want the originally stored W", stored.ValueSemantics.Unit)
	}
	if _, found := store.ByCacheKey(CacheKey(testRef(), analysis, raw, DetectorVersion)); !found {
		t.Error("the profile is not retrievable by its cache key")
	}
}

func TestAnOverrideNeedsAConfirmablePath(t *testing.T) {
	store := NewMemoryStore()
	_, err := store.AppendOverride(ProfileOverride{
		SeriesRef: testRef(), CreatedBy: "user-123",
		FieldPath: "distribution.mean", Action: ActionCorrect, ConfirmedValue: 5,
	})
	if !errors.Is(err, ErrInvalidOverride) {
		t.Fatalf("error = %v, want ErrInvalidOverride: a typo must be refused, not silently ignored", err)
	}
}

func TestACorrectionMustCarryAValue(t *testing.T) {
	store := NewMemoryStore()
	_, err := store.AppendOverride(ProfileOverride{
		SeriesRef: testRef(), CreatedBy: "user-123",
		FieldPath: FieldUnit, Action: ActionCorrect,
	})
	if !errors.Is(err, ErrInvalidOverride) {
		t.Fatalf("error = %v, want ErrInvalidOverride", err)
	}
}

func TestAnOverrideMustNameItsAuthor(t *testing.T) {
	store := NewMemoryStore()
	_, err := store.AppendOverride(ProfileOverride{
		SeriesRef: testRef(),
		FieldPath: FieldUnit, Action: ActionConfirm,
	})
	if !errors.Is(err, ErrInvalidOverride) {
		t.Fatalf("error = %v, want ErrInvalidOverride: the log is an empirical record and needs an author", err)
	}
}

// M1b acceptance: an override survives recomputation. It is keyed by series
// reference, because a new detector version produces a new profile id and an
// override tied to the old one would silently stop applying.
func TestAnOverrideSurvivesRecomputationUnderANewDetectorVersion(t *testing.T) {
	store := NewMemoryStore()
	original := storedProfile(t, store, "1.0.0", "W")

	if _, err := store.AppendOverride(ProfileOverride{
		SeriesRef: original.SeriesRef, ProfileID: original.ProfileID, DetectorVersion: "1.0.0",
		CreatedBy: "user-123", FieldPath: FieldUnit, Action: ActionCorrect,
		ComputedValue: "W", ConfirmedValue: "kW", Note: "the meter reports kilowatts",
	}); err != nil {
		t.Fatalf("AppendOverride: %v", err)
	}

	// A detector improvement: same series, same windows, new version, new id.
	recomputed := storedProfile(t, store, "1.1.0", "W")
	if recomputed.ProfileID == original.ProfileID {
		t.Fatal("the fixture did not actually produce a new profile")
	}

	resolved := Resolve(recomputed, store.Overrides(recomputed.SeriesRef))
	resolution, applied := resolved.Resolution[FieldUnit]
	if !applied {
		t.Fatal("the confirmation did not carry over to the recomputed profile")
	}
	if resolution.ConfirmedValue != "kW" {
		t.Errorf("confirmed_value = %v, want kW", resolution.ConfirmedValue)
	}
	// The body is untouched: the merged form is never stored, so computed and
	// confirmed stay diffable.
	if resolved.ValueSemantics.Unit != "W" {
		t.Errorf("the profile body was mutated to %q; Resolve must only add a resolution map",
			resolved.ValueSemantics.Unit)
	}
}

// The overlay is append-only, so a developer who changes their mind adds a record
// rather than replacing one, and the history stays visible.
func TestASecondOverrideSupersedesTheFirstWithoutErasingIt(t *testing.T) {
	store := NewMemoryStore()
	profile := storedProfile(t, store, DetectorVersion, "W")

	for _, value := range []string{"kW", "MW"} {
		if _, err := store.AppendOverride(ProfileOverride{
			SeriesRef: profile.SeriesRef, ProfileID: profile.ProfileID, CreatedBy: "user-123",
			FieldPath: FieldUnit, Action: ActionCorrect, ComputedValue: "W", ConfirmedValue: value,
			CreatedAt: fixtureStart,
		}); err != nil {
			t.Fatalf("AppendOverride %s: %v", value, err)
		}
	}

	overrides := store.Overrides(profile.SeriesRef)
	if len(overrides) != 2 {
		t.Fatalf("stored overrides = %d, want both records kept", len(overrides))
	}

	resolution := Resolve(profile, overrides).Resolution[FieldUnit]
	if resolution.ConfirmedValue != "MW" {
		t.Errorf("confirmed_value = %v, want the latest, MW", resolution.ConfirmedValue)
	}
	if len(resolution.Supersedes) != 1 {
		t.Errorf("supersedes = %v, want the earlier override named", resolution.Supersedes)
	}
	// The computed value stays what the detector produced, not what the previous
	// correction said.
	if resolution.ComputedValue != "W" {
		t.Errorf("computed_value = %v, want the detector's W", resolution.ComputedValue)
	}
}

func TestOverridesForOtherSeriesAreIgnoredRatherThanRejected(t *testing.T) {
	store := NewMemoryStore()
	profile := storedProfile(t, store, DetectorVersion, "W")

	other := testRef()
	other.VariablePath = "value.total"
	if _, err := store.AppendOverride(ProfileOverride{
		SeriesRef: other, CreatedBy: "user-123", FieldPath: FieldUnit,
		Action: ActionCorrect, ConfirmedValue: "Wh",
	}); err != nil {
		t.Fatalf("AppendOverride: %v", err)
	}

	resolved := Resolve(profile, store.Overrides(other))
	if len(resolved.Resolution) != 0 {
		t.Errorf("resolution = %+v, want nothing applied from another series", resolved.Resolution)
	}
}

func TestARejectionClearsTheFieldRatherThanConfirmingIt(t *testing.T) {
	store := NewMemoryStore()
	profile := storedProfile(t, store, DetectorVersion, "W")

	if _, err := store.AppendOverride(ProfileOverride{
		SeriesRef: profile.SeriesRef, CreatedBy: "user-123",
		FieldPath: FieldUnit, Action: ActionReject, ComputedValue: "W",
	}); err != nil {
		t.Fatalf("AppendOverride: %v", err)
	}

	resolved := Resolve(profile, store.Overrides(profile.SeriesRef))
	value, overridden := resolved.Effective(FieldUnit, "W")
	if !overridden {
		t.Fatal("the rejection was not applied")
	}
	if value != nil {
		t.Errorf("effective value = %v, want nil for a rejection", value)
	}
}

// --- sessions as a separate paginated resource (D27) ---

func TestSessionsArePagedWithACursor(t *testing.T) {
	store := NewMemoryStore()
	analysis, raw := testWindows()
	key := CacheKey(testRef(), analysis, raw, DetectorVersion)

	sessions := make([]Session, 0, 250)
	for i := 0; i < 250; i++ {
		from := fixtureStart.Add(time.Duration(i) * time.Hour)
		sessions = append(sessions, Session{From: from, To: from.Add(30 * time.Minute), DurationS: 1800})
	}
	if _, _, err := store.Put(SeriesProfile{ProfileID: key, SeriesRef: testRef()}, sessions); err != nil {
		t.Fatalf("Put: %v", err)
	}

	first, err := store.Sessions(key, SessionQuery{Limit: 100})
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if first.Total != 250 || len(first.Sessions) != 100 {
		t.Fatalf("page = %d of %d, want 100 of 250", len(first.Sessions), first.Total)
	}
	if first.NextCursor == "" {
		t.Fatal("no cursor was returned for a truncated page")
	}

	second, err := store.Sessions(key, SessionQuery{Limit: 100, Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("Sessions page two: %v", err)
	}
	if !second.Sessions[0].From.Equal(sessions[100].From) {
		t.Errorf("page two starts at %s, want %s", second.Sessions[0].From, sessions[100].From)
	}
}

func TestSessionsCanBeFilteredToAWindow(t *testing.T) {
	store := NewMemoryStore()
	key := "profile-1"
	sessions := []Session{
		{From: fixtureStart, To: fixtureStart.Add(time.Hour)},
		{From: fixtureStart.Add(48 * time.Hour), To: fixtureStart.Add(49 * time.Hour)},
	}
	if _, _, err := store.Put(SeriesProfile{ProfileID: key, SeriesRef: testRef()}, sessions); err != nil {
		t.Fatalf("Put: %v", err)
	}

	page, err := store.Sessions(key, SessionQuery{From: fixtureStart.Add(24 * time.Hour)})
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(page.Sessions) != 1 || !page.Sessions[0].From.Equal(sessions[1].From) {
		t.Errorf("sessions = %+v, want only the later one", page.Sessions)
	}
}

func TestSessionsOfAnUnknownProfileAreNotFound(t *testing.T) {
	store := NewMemoryStore()
	if _, err := store.Sessions("nope", SessionQuery{}); !errors.Is(err, ErrProfileNotFound) {
		t.Fatalf("error = %v, want ErrProfileNotFound", err)
	}
}

func TestAMalformedCursorIsRefused(t *testing.T) {
	store := NewMemoryStore()
	key := "profile-1"
	if _, _, err := store.Put(SeriesProfile{ProfileID: key, SeriesRef: testRef()}, nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := store.Sessions(key, SessionQuery{Cursor: "not-a-number"}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("error = %v, want ErrInvalidCursor", err)
	}
}
