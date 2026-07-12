package icloud

import (
	"fmt"
	"time"

	"github.com/teambition/rrule-go"
)

// maxOccurrencesPerSeries borne le nombre d'occurrences développées pour une
// seule série récurrente, protection contre une RRULE pathologique (ex.
// FREQ=MINUTELY sans UNTIL/COUNT sur une plage de 366 jours).
const maxOccurrencesPerSeries = 2000

// ExpandOccurrences développe un événement récurrent dans [rangeStart, rangeEnd].
// Gère RRULE + EXDATE (exclusions) ; les overrides RECURRENCE-ID remplacent
// l'occurrence générée correspondante (comparaison à la seconde, en UTC) ;
// les overrides tombant dans la plage mais absents de la série générée
// (déplacés hors de leur créneau d'origine) sont tout de même inclus.
// maxOccurrences borne l'expansion ; si <= 0, la valeur par défaut du
// package est utilisée.
func ExpandOccurrences(master Event, overrides []Event, rangeStart, rangeEnd time.Time, maxOccurrences int) ([]Event, error) {
	if maxOccurrences <= 0 {
		maxOccurrences = maxOccurrencesPerSeries
	}

	if master.Recurrence == "" {
		if eventOverlaps(master, rangeStart, rangeEnd) {
			return []Event{master}, nil
		}
		return nil, nil
	}

	ropt, err := rrule.StrToROption(master.Recurrence)
	if err != nil {
		return nil, fmt.Errorf("RRULE invalide (uid=%s) : %w", master.UID, err)
	}
	// NE PAS forcer .UTC() ici : RFC 5545 impose que la récurrence suive
	// l'heure MURALE locale du Dtstart (TZID), pas un instant UTC fixe.
	// Convertir en UTC détruirait la Location et figerait chaque occurrence
	// au même offset UTC que le Dtstart d'origine, ce qui la décale d'1h par
	// rapport à l'heure murale attendue dès qu'un changement DST intervient
	// entre temps. Si l'événement est déjà en Z (UTC), StartTime.Location()
	// est déjà time.UTC, aucune perte d'information.
	ropt.Dtstart = master.StartTime

	rule, err := rrule.NewRRule(*ropt)
	if err != nil {
		return nil, fmt.Errorf("RRULE invalide (uid=%s) : %w", master.UID, err)
	}

	set := &rrule.Set{}
	set.RRule(rule)
	for _, ex := range master.exDates {
		set.ExDate(ex.UTC())
	}

	// duration DOIT être calculée avant l'appel à Between : elle sert à
	// élargir la borne basse (voir ci-dessous, FIX-D) pour ne pas perdre les
	// occurrences qui commencent avant rangeStart mais débordent dedans.
	duration := master.EndTime.Sub(master.StartTime)

	// Borne basse élargie de `duration` : une occurrence qui démarre avant
	// rangeStart peut quand même chevaucher la plage si elle déborde dedans
	// (ex. créneau nocturne 22h->2h). set.Between filtre par heure de
	// DÉBUT, sans cet élargissement, ces occurrences seraient perdues alors
	// que le chemin non-récurrent (eventOverlaps) les inclurait. La borne
	// haute reste inc=true côté rrule-go (elle sur-sélectionne large), le
	// filtre eventOverlaps ci-dessous ramène la sémantique au demi-ouvert
	// [rangeStart, rangeEnd) utilisé partout ailleurs.
	lowerBound := rangeStart.Add(-duration)
	occTimes := set.Between(lowerBound.UTC(), rangeEnd.UTC(), true)
	if len(occTimes) > maxOccurrences {
		occTimes = occTimes[:maxOccurrences]
	}

	overrideByRecID := make(map[int64]Event, len(overrides))
	for _, o := range overrides {
		overrideByRecID[o.recurrenceID.UTC().Unix()] = o
	}
	used := make(map[int64]bool, len(overrideByRecID))

	out := make([]Event, 0, len(occTimes))
	for _, occ := range occTimes {
		// Ne garder que les occurrences qui chevauchent réellement
		// [rangeStart, rangeEnd) une fois reconstruites avec leur durée
		// pleine, la borne basse élargie ci-dessus sur-sélectionne
		// volontairement, ce filtre ramène à la sémantique exacte de
		// eventOverlaps (cohérente avec le chemin non-récurrent).
		if !eventOverlaps(Event{StartTime: occ, EndTime: occ.Add(duration)}, rangeStart, rangeEnd) {
			continue
		}
		key := occ.UTC().Unix()
		if ov, ok := overrideByRecID[key]; ok {
			out = append(out, ov)
			used[key] = true
			continue
		}
		clone := master
		clone.StartTime = occ
		clone.EndTime = occ.Add(duration)
		clone.exDates = nil
		out = append(out, clone)
	}

	// Overrides tombant dans la plage mais absents de la série générée par
	// rrule-go (ex. déplacés hors de leur créneau d'origine).
	for key, ov := range overrideByRecID {
		if used[key] {
			continue
		}
		if eventOverlaps(ov, rangeStart, rangeEnd) {
			out = append(out, ov)
		}
	}

	if len(out) > maxOccurrences {
		out = out[:maxOccurrences]
	}

	return out, nil
}

// eventOverlaps teste le chevauchement demi-ouvert [start,end) d'un
// événement avec [rangeStart, rangeEnd].
func eventOverlaps(e Event, rangeStart, rangeEnd time.Time) bool {
	return e.StartTime.Before(rangeEnd) && e.EndTime.After(rangeStart)
}
