package icloud

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"golang.org/x/time/rate"
)

// GuardedService décore un Service avec rate limiting (deux budgets
// indépendants : lecture et écriture) et retry borné avec backoff
// exponentiel. Seules les opérations idempotentes sont retryées
// (lectures + DeleteEvent) ; CreateEvent/UpdateEvent ne le sont JAMAIS
// (non idempotentes : un retry créerait un doublon ou incrémenterait
// SEQUENCE à tort).
type GuardedService struct {
	inner      Service
	readLimit  *rate.Limiter // 60 lectures/min, burst 10
	writeLimit *rate.Limiter // 20 écritures/min, burst 3
	maxRetries int
	baseDelay  time.Duration
}

var _ Service = (*GuardedService)(nil)

// NewGuardedService construit un GuardedService.
func NewGuardedService(inner Service, maxRetries int, baseDelay time.Duration) *GuardedService {
	return &GuardedService{
		inner:      inner,
		readLimit:  rate.NewLimiter(rate.Every(time.Minute/60), 10),
		writeLimit: rate.NewLimiter(rate.Every(time.Minute/20), 3),
		maxRetries: maxRetries,
		baseDelay:  baseDelay,
	}
}

func (g *GuardedService) waitRead(ctx context.Context) error {
	if err := g.readLimit.Wait(ctx); err != nil {
		return fmt.Errorf("limite de débit (lecture) dépassée : %w", err)
	}
	return nil
}

func (g *GuardedService) waitWrite(ctx context.Context) error {
	if err := g.writeLimit.Wait(ctx); err != nil {
		return fmt.Errorf("limite de débit (écriture) dépassée : %w", err)
	}
	return nil
}

// retry réessaie fn jusqu'à maxRetries fois avec un backoff exponentiel
// (baseDelay * 2^tentative), borné par ctx.Done().
func (g *GuardedService) retry(ctx context.Context, op string, fn func() error) error {
	var lastErr error
	for attempt := 0; attempt <= g.maxRetries; attempt++ {
		lastErr = fn()
		if lastErr == nil {
			return nil
		}
		if attempt == g.maxRetries {
			break
		}
		delay := g.baseDelay * time.Duration(math.Pow(2, float64(attempt)))
		slog.Warn("nouvelle tentative", "operation", op, "tentative", attempt+1, "delai", delay, "erreur", lastErr)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return lastErr
}

// ListCalendars, lecture, retryée.
func (g *GuardedService) ListCalendars(ctx context.Context) ([]Calendar, error) {
	if err := g.waitRead(ctx); err != nil {
		return nil, err
	}
	var result []Calendar
	err := g.retry(ctx, "ListCalendars", func() error {
		var e error
		result, e = g.inner.ListCalendars(ctx)
		return e
	})
	return result, err
}

// SearchEvents, lecture, retryée.
func (g *GuardedService) SearchEvents(ctx context.Context, calendarPath string, start, end time.Time) ([]Event, error) {
	if err := g.waitRead(ctx); err != nil {
		return nil, err
	}
	var result []Event
	err := g.retry(ctx, "SearchEvents", func() error {
		var e error
		result, e = g.inner.SearchEvents(ctx, calendarPath, start, end)
		return e
	})
	return result, err
}

// CreateEvent, écriture, JAMAIS retryée (non idempotente).
func (g *GuardedService) CreateEvent(ctx context.Context, calendarPath string, ev *NewEvent) (string, error) {
	if err := g.waitWrite(ctx); err != nil {
		return "", err
	}
	return g.inner.CreateEvent(ctx, calendarPath, ev)
}

// UpdateEvent, écriture, JAMAIS retryée (non idempotente : SEQUENCE/DTSTAMP
// changent à chaque tentative).
func (g *GuardedService) UpdateEvent(ctx context.Context, calendarPath, uid string, up *EventUpdate) error {
	if err := g.waitWrite(ctx); err != nil {
		return err
	}
	return g.inner.UpdateEvent(ctx, calendarPath, uid, up)
}

// DeleteEvent, écriture, retryée (idempotente : supprimer un événement déjà
// supprimé échoue proprement sans effet de bord dangereux).
func (g *GuardedService) DeleteEvent(ctx context.Context, calendarPath, uid string) (string, error) {
	if err := g.waitWrite(ctx); err != nil {
		return "", err
	}
	var title string
	err := g.retry(ctx, "DeleteEvent", func() error {
		var e error
		title, e = g.inner.DeleteEvent(ctx, calendarPath, uid)
		return e
	})
	return title, err
}
