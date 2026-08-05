package sessionstore

import (
	"context"
	"testing"
	"time"

	adksession "google.golang.org/adk/v2/session"
)

func TestManagedServiceAppendStats(t *testing.T) {
	ctx := context.Background()
	svc, err := NewInMemoryManagedService(Config{MaxEvents: 1})
	if err != nil {
		t.Fatalf("NewInMemoryManagedService() error = %v", err)
	}

	var _ adksession.Service = svc

	created, err := svc.Create(ctx, &adksession.CreateRequest{
		AppName:   "app",
		UserID:    "user",
		SessionID: "session",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if err := svc.AppendEvent(ctx, created.Session, &adksession.Event{Author: "user"}); err != nil {
		t.Fatalf("AppendEvent(1) error = %v", err)
	}
	if err := svc.AppendEvent(ctx, created.Session, &adksession.Event{Author: "agent"}); err != nil {
		t.Fatalf("AppendEvent(2) error = %v", err)
	}

	st, ok := svc.Stats("app", "user", "session")
	if !ok {
		t.Fatal("Stats() missing session")
	}
	if st.EventCount != 2 {
		t.Fatalf("EventCount = %d, want 2", st.EventCount)
	}
	if !st.OverLimit {
		t.Fatal("OverLimit = false, want true")
	}
}

func TestManagedServiceGetLimitsRecentEvents(t *testing.T) {
	ctx := context.Background()
	svc, err := NewInMemoryManagedService(Config{MaxEvents: 2})
	if err != nil {
		t.Fatalf("NewInMemoryManagedService() error = %v", err)
	}

	created, err := svc.Create(ctx, &adksession.CreateRequest{
		AppName:   "app",
		UserID:    "user",
		SessionID: "limited",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	for _, author := range []string{"first", "second", "third"} {
		if err := svc.AppendEvent(ctx, created.Session, &adksession.Event{Author: author}); err != nil {
			t.Fatalf("AppendEvent(%s) error = %v", author, err)
		}
	}

	got, err := svc.Get(ctx, &adksession.GetRequest{AppName: "app", UserID: "user", SessionID: "limited"})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Session.Events().Len() != 2 {
		t.Fatalf("Events().Len() = %d, want 2", got.Session.Events().Len())
	}
	if got.Session.Events().At(0).Author != "second" || got.Session.Events().At(1).Author != "third" {
		t.Fatalf("returned authors = %q, %q; want second, third", got.Session.Events().At(0).Author, got.Session.Events().At(1).Author)
	}
	st, ok := svc.Stats("app", "user", "limited")
	if !ok {
		t.Fatal("Stats() missing session")
	}
	if st.EventCount != 3 {
		t.Fatalf("Stats().EventCount = %d, want 3", st.EventCount)
	}
	if !st.OverLimit {
		t.Fatal("Stats().OverLimit = false, want true")
	}
}

func TestManagedServiceGetLimitsRecentTurns(t *testing.T) {
	ctx := context.Background()
	svc, err := NewInMemoryManagedService(Config{MaxTurns: 2})
	if err != nil {
		t.Fatalf("NewInMemoryManagedService() error = %v", err)
	}

	created, err := svc.Create(ctx, &adksession.CreateRequest{
		AppName:   "app",
		UserID:    "user",
		SessionID: "turns",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	for _, author := range []string{"user", "agent", "user", "agent", "user", "agent"} {
		if err := svc.AppendEvent(ctx, created.Session, &adksession.Event{Author: author}); err != nil {
			t.Fatalf("AppendEvent(%s) error = %v", author, err)
		}
	}

	got, err := svc.Get(ctx, &adksession.GetRequest{AppName: "app", UserID: "user", SessionID: "turns"})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Session.Events().Len() != 4 {
		t.Fatalf("Events().Len() = %d, want 4", got.Session.Events().Len())
	}
	for i, want := range []string{"user", "agent", "user", "agent"} {
		if got.Session.Events().At(i).Author != want {
			t.Fatalf("event[%d].Author = %q, want %q", i, got.Session.Events().At(i).Author, want)
		}
	}
	if err := svc.AppendEvent(ctx, got.Session, &adksession.Event{Author: "agent"}); err != nil {
		t.Fatalf("AppendEvent(windowed session) error = %v", err)
	}
	st, ok := svc.Stats("app", "user", "turns")
	if !ok {
		t.Fatal("Stats() missing session")
	}
	if st.EventCount != 7 {
		t.Fatalf("Stats().EventCount = %d, want 7", st.EventCount)
	}
	if st.TurnCount != 3 {
		t.Fatalf("Stats().TurnCount = %d, want 3", st.TurnCount)
	}
	if !st.OverLimit {
		t.Fatal("Stats().OverLimit = false, want true")
	}
}

func TestManagedServiceDeleteExpired(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	svc, err := NewInMemoryManagedService(Config{
		TTL: time.Second,
		Now: func() time.Time { return now.Add(2 * time.Second) },
	})
	if err != nil {
		t.Fatalf("NewInMemoryManagedService() error = %v", err)
	}

	created, err := svc.Create(ctx, &adksession.CreateRequest{
		AppName:   "app",
		UserID:    "user",
		SessionID: "expired",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := svc.AppendEvent(ctx, created.Session, &adksession.Event{Author: "user", Timestamp: now}); err != nil {
		t.Fatalf("AppendEvent() error = %v", err)
	}

	deleted, err := svc.DeleteExpired(ctx, "app", "user")
	if err != nil {
		t.Fatalf("DeleteExpired() error = %v", err)
	}
	if deleted != 1 {
		t.Fatalf("DeleteExpired() = %d, want 1", deleted)
	}
	if _, err := svc.Get(ctx, &adksession.GetRequest{AppName: "app", UserID: "user", SessionID: "expired"}); err == nil {
		t.Fatal("Get() after DeleteExpired() error = nil, want not found")
	}
	if _, ok := svc.Stats("app", "user", "expired"); ok {
		t.Fatal("Stats() still has expired session")
	}
}

// The ADK runner does Get() -> AppendEvent(userEvent) -> read Session().Events()
// while building the model request. A windowed session must therefore reflect
// events appended after Get(); otherwise the current user message is invisible
// to the model and ADK substitutes a synthetic continuation prompt for it.
func TestManagedServiceWindowedSessionSeesLaterAppends(t *testing.T) {
	ctx := context.Background()
	svc, err := NewInMemoryManagedService(Config{MaxTurns: 3})
	if err != nil {
		t.Fatalf("NewInMemoryManagedService() error = %v", err)
	}
	created, err := svc.Create(ctx, &adksession.CreateRequest{
		AppName:   "app",
		UserID:    "user",
		SessionID: "live",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	for _, author := range []string{"user", "agent"} {
		if err := svc.AppendEvent(ctx, created.Session, &adksession.Event{Author: author}); err != nil {
			t.Fatalf("AppendEvent(%s) error = %v", author, err)
		}
	}

	got, err := svc.Get(ctx, &adksession.GetRequest{AppName: "app", UserID: "user", SessionID: "live"})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if n := got.Session.Events().Len(); n != 2 {
		t.Fatalf("Events().Len() before append = %d, want 2", n)
	}

	// Simulate the runner appending the current turn's user message.
	if err := svc.AppendEvent(ctx, got.Session, &adksession.Event{Author: "user"}); err != nil {
		t.Fatalf("AppendEvent(user) error = %v", err)
	}

	events := got.Session.Events()
	if n := events.Len(); n != 3 {
		t.Fatalf("Events().Len() after append = %d, want 3", n)
	}
	if last := events.At(events.Len() - 1); last == nil || last.Author != "user" {
		t.Fatalf("last event author = %v, want \"user\"", last)
	}
}

func TestManagedServiceDeleteExpiredAllUsers(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	svc, err := NewInMemoryManagedService(Config{
		TTL: time.Second,
		Now: func() time.Time {
			return now.Add(2 * time.Second)
		},
	})
	if err != nil {
		t.Fatalf("NewInMemoryManagedService() error = %v", err)
	}

	for _, userID := range []string{"alice", "bob"} {
		created, err := svc.Create(ctx, &adksession.CreateRequest{
			AppName:   "app",
			UserID:    userID,
			SessionID: "expired",
		})
		if err != nil {
			t.Fatalf("Create(%s) error = %v", userID, err)
		}
		if err := svc.AppendEvent(ctx, created.Session, &adksession.Event{
			Author:    "user",
			Timestamp: now,
		}); err != nil {
			t.Fatalf("AppendEvent(%s) error = %v", userID, err)
		}
	}

	deleted, err := svc.DeleteExpired(ctx, "app", "")
	if err != nil {
		t.Fatalf("DeleteExpired() error = %v", err)
	}
	if deleted != 2 {
		t.Fatalf("DeleteExpired() = %d, want 2", deleted)
	}
}
