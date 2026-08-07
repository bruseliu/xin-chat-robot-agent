package sessionstore

import (
	"context"
	"testing"

	adksession "google.golang.org/adk/v2/session"
)

type getRequestSpy struct {
	adksession.Service
	request *adksession.GetRequest
}

func (spy *getRequestSpy) Get(ctx context.Context, request *adksession.GetRequest) (*adksession.GetResponse, error) {
	copy := *request
	spy.request = &copy
	return spy.Service.Get(ctx, request)
}

func TestManagedServiceAppliesEventAndTurnLimitsTogether(t *testing.T) {
	ctx := context.Background()
	inner := &getRequestSpy{Service: adksession.InMemoryService()}
	svc, err := NewManagedService(inner, Config{MaxEvents: 6, MaxTurns: 2})
	if err != nil {
		t.Fatalf("NewManagedService() error = %v", err)
	}
	created, err := svc.Create(ctx, &adksession.CreateRequest{AppName: "app", UserID: "user", SessionID: "bounded"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := svc.AppendEvent(ctx, created.Session, &adksession.Event{Author: "user"}); err != nil {
			t.Fatalf("AppendEvent(user %d) error = %v", i, err)
		}
		if err := svc.AppendEvent(ctx, created.Session, &adksession.Event{Author: "agent"}); err != nil {
			t.Fatalf("AppendEvent(agent %d) error = %v", i, err)
		}
	}
	got, err := svc.Get(ctx, &adksession.GetRequest{AppName: "app", UserID: "user", SessionID: "bounded"})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if inner.request == nil || inner.request.NumRecentEvents != 6 {
		t.Fatalf("inner NumRecentEvents = %v, want 6", inner.request)
	}
	if got.Session.Events().Len() != 4 {
		t.Fatalf("visible events = %d, want two complete turns", got.Session.Events().Len())
	}
}
