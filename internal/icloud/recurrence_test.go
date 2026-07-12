package icloud

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q : %v", s, err)
	}
	return tm
}

func TestExpandOccurrences_NonRecurring(t *testing.T) {
	master := Event{
		UID:       "uid-1",
		Title:     "Simple",
		StartTime: mustParse(t, "2026-07-06T09:00:00Z"),
		EndTime:   mustParse(t, "2026-07-06T10:00:00Z"),
	}

	t.Run("dans la plage", func(t *testing.T) {
		out, err := ExpandOccurrences(master, nil, mustParse(t, "2026-07-01T00:00:00Z"), mustParse(t, "2026-07-08T00:00:00Z"), 0)
		if err != nil {
			t.Fatalf("erreur inattendue : %v", err)
		}
		if len(out) != 1 {
			t.Fatalf("len = %d, want 1", len(out))
		}
	})

	t.Run("hors plage", func(t *testing.T) {
		out, err := ExpandOccurrences(master, nil, mustParse(t, "2026-08-01T00:00:00Z"), mustParse(t, "2026-08-08T00:00:00Z"), 0)
		if err != nil {
			t.Fatalf("erreur inattendue : %v", err)
		}
		if len(out) != 0 {
			t.Fatalf("len = %d, want 0", len(out))
		}
	})
}

func TestExpandOccurrences_DailyWithCount(t *testing.T) {
	master := Event{
		UID:        "uid-daily",
		Title:      "Quotidien",
		StartTime:  mustParse(t, "2026-07-01T09:00:00Z"),
		EndTime:    mustParse(t, "2026-07-01T10:00:00Z"),
		Recurrence: "FREQ=DAILY;COUNT=5",
	}

	out, err := ExpandOccurrences(master, nil, mustParse(t, "2026-07-01T00:00:00Z"), mustParse(t, "2026-07-31T00:00:00Z"), 0)
	if err != nil {
		t.Fatalf("erreur inattendue : %v", err)
	}
	if len(out) != 5 {
		t.Fatalf("len = %d, want 5", len(out))
	}
	for i, ev := range out {
		wantDay := 1 + i
		if ev.StartTime.Day() != wantDay {
			t.Errorf("occurrence %d : jour = %d, want %d", i, ev.StartTime.Day(), wantDay)
		}
		if ev.StartTime.Hour() != 9 {
			t.Errorf("occurrence %d : heure = %d, want 9", i, ev.StartTime.Hour())
		}
	}
}

func TestExpandOccurrences_WeeklyWithExdate(t *testing.T) {
	exDate := mustParse(t, "2026-07-13T18:00:00Z")
	master := Event{
		UID:        "uid-weekly",
		Title:      "Hebdo",
		StartTime:  mustParse(t, "2026-07-06T18:00:00Z"),
		EndTime:    mustParse(t, "2026-07-06T19:00:00Z"),
		Recurrence: "FREQ=WEEKLY;COUNT=5",
		exDates:    []time.Time{exDate},
	}

	out, err := ExpandOccurrences(master, nil, mustParse(t, "2026-07-01T00:00:00Z"), mustParse(t, "2026-08-15T00:00:00Z"), 0)
	if err != nil {
		t.Fatalf("erreur inattendue : %v", err)
	}
	// 5 occurrences moins l'EXDATE du 13 juillet = 4.
	if len(out) != 4 {
		t.Fatalf("len = %d, want 4 : %+v", len(out), out)
	}
	for _, ev := range out {
		if ev.StartTime.Equal(exDate) {
			t.Errorf("l'occurrence exclue (EXDATE) est présente : %v", ev.StartTime)
		}
	}
}

func TestExpandOccurrences_OverrideReplacesOccurrence(t *testing.T) {
	recID := mustParse(t, "2026-07-13T14:00:00Z")
	master := Event{
		UID:        "uid-override",
		Title:      "Suivi",
		StartTime:  mustParse(t, "2026-07-06T14:00:00Z"),
		EndTime:    mustParse(t, "2026-07-06T15:00:00Z"),
		Recurrence: "FREQ=WEEKLY;COUNT=4",
	}
	override := Event{
		UID:          "uid-override",
		Title:        "Suivi (déplacé)",
		StartTime:    mustParse(t, "2026-07-13T16:00:00Z"),
		EndTime:      mustParse(t, "2026-07-13T17:00:00Z"),
		recurrenceID: recID,
	}

	out, err := ExpandOccurrences(master, []Event{override}, mustParse(t, "2026-07-01T00:00:00Z"), mustParse(t, "2026-08-15T00:00:00Z"), 0)
	if err != nil {
		t.Fatalf("erreur inattendue : %v", err)
	}
	if len(out) != 4 {
		t.Fatalf("len = %d, want 4 : %+v", len(out), out)
	}

	var found bool
	for _, ev := range out {
		if ev.StartTime.Equal(mustParse(t, "2026-07-13T16:00:00Z")) {
			found = true
			if ev.Title != "Suivi (déplacé)" {
				t.Errorf("Title = %q, want override title", ev.Title)
			}
		}
		if ev.StartTime.Equal(mustParse(t, "2026-07-13T14:00:00Z")) {
			t.Errorf("l'occurrence d'origine (remplacée par l'override) ne devrait pas apparaître")
		}
	}
	if !found {
		t.Errorf("override non trouvé dans les résultats : %+v", out)
	}
}

// TestExpandOccurrences_PreservesTimezoneAcrossDST, FIX-C. Un événement
// hebdomadaire à 10h heure murale America/New_York doit rester à 10h heure
// murale à travers un changement DST (fin de la DST US le 01/11/2026), donc
// à un offset UTC différent avant/après (-04:00 puis -05:00 → 14:00Z puis
// 15:00Z). Forcer .UTC() sur le Dtstart avant expansion figerait l'occurrence
// à un offset UTC constant, ce qui la décale d'1h côté mur après le
// changement DST, bug RFC 5545 (l'heure de récurrence doit être l'heure
// murale locale, pas un instant UTC fixe).
func TestExpandOccurrences_PreservesTimezoneAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation : %v", err)
	}
	start := time.Date(2026, 10, 5, 10, 0, 0, 0, loc) // lundi 10h heure murale NY

	// Occurrence exclue (EXDATE) : simule EXDATE;TZID=America/New_York:20261019T100000
	// tel que résolu par ical.go (déjà converti en instant UTC absolu à ce
	// stade, parseExDateProp fait la conversion TZID→UTC en amont).
	excludedWallClock := time.Date(2026, 10, 19, 10, 0, 0, 0, loc)

	master := Event{
		UID:        "uid-dst",
		Title:      "Hebdo NY",
		StartTime:  start,
		EndTime:    start.Add(time.Hour),
		Recurrence: "FREQ=WEEKLY;COUNT=6",
		exDates:    []time.Time{excludedWallClock.UTC()},
	}

	rangeStart := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	rangeEnd := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	out, err := ExpandOccurrences(master, nil, rangeStart, rangeEnd, 0)
	if err != nil {
		t.Fatalf("erreur inattendue : %v", err)
	}
	// 6 occurrences hebdo moins l'EXDATE du 19 octobre = 5.
	if len(out) != 5 {
		t.Fatalf("len = %d, want 5 : %+v", len(out), out)
	}

	dstCutoff := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, occ := range out {
		if occ.StartTime.Equal(excludedWallClock.UTC()) {
			t.Errorf("l'occurrence exclue (EXDATE, 19/10 10h) est présente : %v", occ.StartTime)
		}
		wallHour := occ.StartTime.In(loc).Hour()
		if wallHour != 10 {
			t.Errorf("occurrence %v : heure murale America/New_York = %d, want 10 (préservée à travers DST)", occ.StartTime, wallHour)
		}
		wantUTCHour := 14 // EDT (-04:00) avant le changement DST
		if !occ.StartTime.Before(dstCutoff) {
			wantUTCHour = 15 // EST (-05:00) après le changement DST
		}
		if got := occ.StartTime.UTC().Hour(); got != wantUTCHour {
			t.Errorf("occurrence %v : heure UTC = %d, want %d (offset DST correct)", occ.StartTime, got, wantUTCHour)
		}
	}
}

// TestExpandOccurrences_IncludesOccurrenceOverlappingRangeStart, FIX-D. Une
// occurrence récurrente nocturne (22h -> 2h, chevauche minuit) dont une
// instance démarre la veille de rangeStart mais déborde dedans (sa fin est
// après rangeStart) doit être incluse, cohérence avec eventOverlaps, déjà
// utilisé pour le chemin non-récurrent.
func TestExpandOccurrences_IncludesOccurrenceOverlappingRangeStart(t *testing.T) {
	master := Event{
		UID:        "uid-overnight",
		Title:      "Garde de nuit",
		StartTime:  mustParse(t, "2026-07-06T22:00:00Z"), // lundi 22h
		EndTime:    mustParse(t, "2026-07-07T02:00:00Z"), // mardi 2h (4h de durée)
		Recurrence: "FREQ=WEEKLY;COUNT=4",
	}

	// La plage démarre après le début de l'occurrence du 6 juillet mais
	// avant sa fin (qui déborde après minuit) : l'occurrence chevauche le
	// début de la plage et doit être incluse.
	rangeStart := mustParse(t, "2026-07-07T01:00:00Z")
	rangeEnd := mustParse(t, "2026-08-10T00:00:00Z")

	out, err := ExpandOccurrences(master, nil, rangeStart, rangeEnd, 0)
	if err != nil {
		t.Fatalf("erreur inattendue : %v", err)
	}

	var found bool
	for _, ev := range out {
		if ev.StartTime.Equal(mustParse(t, "2026-07-06T22:00:00Z")) {
			found = true
		}
	}
	if !found {
		t.Fatalf("occurrence chevauchant le début de plage (06/07 22:00 -> 07/07 02:00) absente : %+v", out)
	}
}

func TestExpandOccurrences_InfiniteRRuleBoundedByRange(t *testing.T) {
	master := Event{
		UID:        "uid-infinite",
		Title:      "Sans fin",
		StartTime:  mustParse(t, "2026-01-01T09:00:00Z"),
		EndTime:    mustParse(t, "2026-01-01T09:30:00Z"),
		Recurrence: "FREQ=DAILY", // ni UNTIL ni COUNT
	}

	start := mustParse(t, "2026-07-01T00:00:00Z")
	end := mustParse(t, "2026-07-11T00:00:00Z") // 10 jours
	out, err := ExpandOccurrences(master, nil, start, end, 0)
	if err != nil {
		t.Fatalf("erreur inattendue : %v", err)
	}
	if len(out) != 10 {
		t.Fatalf("len = %d, want 10 (bornée par la plage demandée)", len(out))
	}
}

func TestExpandOccurrences_MaxOccurrencesTruncates(t *testing.T) {
	master := Event{
		UID:        "uid-truncate",
		Title:      "Quotidien long",
		StartTime:  mustParse(t, "2026-01-01T09:00:00Z"),
		EndTime:    mustParse(t, "2026-01-01T09:30:00Z"),
		Recurrence: "FREQ=DAILY",
	}

	start := mustParse(t, "2026-01-01T00:00:00Z")
	end := mustParse(t, "2027-01-01T00:00:00Z") // 365 jours
	out, err := ExpandOccurrences(master, nil, start, end, 5)
	if err != nil {
		t.Fatalf("erreur inattendue : %v", err)
	}
	if len(out) != 5 {
		t.Fatalf("len = %d, want 5 (tronqué par maxOccurrences)", len(out))
	}
}

func TestExpandOccurrences_InvalidRRule(t *testing.T) {
	master := Event{
		UID:        "uid-invalid",
		StartTime:  mustParse(t, "2026-01-01T09:00:00Z"),
		EndTime:    mustParse(t, "2026-01-01T09:30:00Z"),
		Recurrence: "CECI-N-EST-PAS-UNE-RRULE-VALIDE",
	}

	_, err := ExpandOccurrences(master, nil, mustParse(t, "2026-01-01T00:00:00Z"), mustParse(t, "2026-02-01T00:00:00Z"), 0)
	if err == nil {
		t.Fatal("attendu : erreur RRULE invalide")
	}
}
