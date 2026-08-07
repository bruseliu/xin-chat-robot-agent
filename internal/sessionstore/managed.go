package sessionstore

import (
	"context"
	"fmt"
	"iter"
	"sync"
	"time"

	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

type Config struct {
	TTL       time.Duration
	MaxEvents int
	MaxTurns  int
	Now       func() time.Time
}

type ManagedService struct {
	inner adksession.Service
	cfg   Config

	mu    sync.RWMutex
	stats map[string]Stats
}

type Stats struct {
	AppName        string
	UserID         string
	SessionID      string
	EventCount     int
	TurnCount      int
	LastUpdateTime time.Time
	OverLimit      bool
}

func NewManagedService(inner adksession.Service, cfg Config) (*ManagedService, error) {
	if inner == nil {
		return nil, fmt.Errorf("sessionstore: inner service is required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &ManagedService{
		inner: inner,
		cfg:   cfg,
		stats: make(map[string]Stats),
	}, nil
}

func NewInMemoryManagedService(cfg Config) (*ManagedService, error) {
	return NewManagedService(adksession.InMemoryService(), cfg)
}

func (s *ManagedService) Create(ctx context.Context, req *adksession.CreateRequest) (*adksession.CreateResponse, error) {
	resp, err := s.inner.Create(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp != nil && resp.Session != nil {
		s.record(resp.Session)
	}
	return resp, nil
}

func (s *ManagedService) Get(ctx context.Context, req *adksession.GetRequest) (*adksession.GetResponse, error) {
	innerReq, fullView := s.innerGetRequest(req)
	resp, err := s.inner.Get(ctx, innerReq)
	if err != nil {
		return nil, err
	}
	if resp != nil && resp.Session != nil {
		if fullView {
			s.record(resp.Session)
		}
		resp.Session = s.windowSession(resp.Session)
	}
	return resp, nil
}

func (s *ManagedService) List(ctx context.Context, req *adksession.ListRequest) (*adksession.ListResponse, error) {
	return s.inner.List(ctx, req)
}

func (s *ManagedService) Delete(ctx context.Context, req *adksession.DeleteRequest) error {
	if err := s.inner.Delete(ctx, req); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.stats, key(req.AppName, req.UserID, req.SessionID))
	s.mu.Unlock()
	return nil
}

func (s *ManagedService) AppendEvent(ctx context.Context, sess adksession.Session, event *adksession.Event) error {
	if sess == nil {
		return fmt.Errorf("sessionstore: session is nil")
	}
	if event == nil {
		return fmt.Errorf("sessionstore: event is nil")
	}
	// Unwrap sessionView so the inner service receives the concrete *session
	// it can mutate in-place. This ensures the caller's session reference
	// reflects the newly appended event (critical for ADK runner correctness).
	target := sess
	if sv, ok := sess.(sessionView); ok {
		target = sv.inner
	}
	if err := s.inner.AppendEvent(ctx, target, event); err != nil {
		return err
	}
	s.record(target)
	return nil
}

func (s *ManagedService) innerGetRequest(req *adksession.GetRequest) (*adksession.GetRequest, bool) {
	if req == nil {
		return nil, false
	}
	fullView := req.NumRecentEvents == 0 && req.After.IsZero()
	if s.cfg.MaxEvents <= 0 {
		return req, fullView
	}
	limited := *req
	if limited.NumRecentEvents == 0 || limited.NumRecentEvents > s.cfg.MaxEvents {
		limited.NumRecentEvents = s.cfg.MaxEvents
	}
	return &limited, false
}

func (s *ManagedService) windowSession(sess adksession.Session) adksession.Session {
	if sess == nil || s.cfg.MaxTurns <= 0 {
		return sess
	}
	return sessionView{inner: sess, maxTurns: s.cfg.MaxTurns}
}

func (s *ManagedService) Stats(appName, userID, sessionID string) (Stats, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.stats[key(appName, userID, sessionID)]
	return st, ok
}

func (s *ManagedService) DeleteExpired(ctx context.Context, appName, userID string) (int, error) {
	if s.cfg.TTL <= 0 {
		return 0, nil
	}
	resp, err := s.inner.List(ctx, &adksession.ListRequest{AppName: appName, UserID: userID})
	if err != nil {
		return 0, err
	}
	now := s.cfg.Now()
	deleted := 0
	for _, sess := range resp.Sessions {
		if sess == nil || now.Sub(sess.LastUpdateTime()) <= s.cfg.TTL {
			continue
		}
		if err := s.Delete(ctx, &adksession.DeleteRequest{
			AppName:   sess.AppName(),
			UserID:    sess.UserID(),
			SessionID: sess.ID(),
		}); err != nil {
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
}

func (s *ManagedService) Inner() adksession.Service {
	return s.inner
}

func (s *ManagedService) record(sess adksession.Session) {
	if sess == nil {
		return
	}
	events := collectEvents(sess.Events())
	eventCount := len(events)
	turnCount := countTurns(events)
	st := Stats{
		AppName:        sess.AppName(),
		UserID:         sess.UserID(),
		SessionID:      sess.ID(),
		EventCount:     eventCount,
		TurnCount:      turnCount,
		LastUpdateTime: sess.LastUpdateTime(),
		OverLimit:      (s.cfg.MaxEvents > 0 && eventCount > s.cfg.MaxEvents) || (s.cfg.MaxTurns > 0 && turnCount > s.cfg.MaxTurns),
	}
	s.mu.Lock()
	s.stats[key(st.AppName, st.UserID, st.SessionID)] = st
	s.mu.Unlock()
}

func collectEvents(events adksession.Events) []*adksession.Event {
	if events == nil {
		return nil
	}
	items := make([]*adksession.Event, 0, events.Len())
	for event := range events.All() {
		items = append(items, event)
	}
	return items
}

func keepLastTurns(events []*adksession.Event, maxTurns int) []*adksession.Event {
	if maxTurns <= 0 || len(events) == 0 {
		return events
	}
	turns := 0
	start := 0
	for i := len(events) - 1; i >= 0; i-- {
		if !isUserEvent(events[i]) {
			continue
		}
		turns++
		if turns == maxTurns {
			start = i
			break
		}
	}
	if turns < maxTurns {
		return events
	}
	return events[start:]
}

func countTurns(events []*adksession.Event) int {
	turns := 0
	for _, event := range events {
		if isUserEvent(event) {
			turns++
		}
	}
	return turns
}

func isUserEvent(event *adksession.Event) bool {
	if event == nil {
		return false
	}
	if event.Author == genai.RoleUser {
		return true
	}
	return event.LLMResponse.Content != nil && event.LLMResponse.Content.Role == genai.RoleUser
}

// sessionView presents a turn-windowed view of an underlying session.
//
// The window must be computed lazily on each Events() call rather than snapshot
// at construction time. The ADK runner does Get() -> AppendEvent(userEvent) ->
// read Session().Events() when building the model request; a frozen snapshot
// would omit the just-appended user message, and ADK would then treat the
// history as ending on a model turn and substitute a synthetic English
// continuation prompt in place of what the user actually said.
type sessionView struct {
	inner    adksession.Session
	maxTurns int
}

func (s sessionView) ID() string {
	return s.inner.ID()
}

func (s sessionView) AppName() string {
	return s.inner.AppName()
}

func (s sessionView) UserID() string {
	return s.inner.UserID()
}

func (s sessionView) State() adksession.State {
	return s.inner.State()
}

func (s sessionView) Events() adksession.Events {
	return eventList(keepLastTurns(collectEvents(s.inner.Events()), s.maxTurns))
}

func (s sessionView) LastUpdateTime() time.Time {
	return s.inner.LastUpdateTime()
}

type eventList []*adksession.Event

func (e eventList) All() iter.Seq[*adksession.Event] {
	return func(yield func(*adksession.Event) bool) {
		for _, event := range e {
			if !yield(event) {
				return
			}
		}
	}
}

func (e eventList) Len() int {
	return len(e)
}

func (e eventList) At(i int) *adksession.Event {
	if i >= 0 && i < len(e) {
		return e[i]
	}
	return nil
}

func key(appName, userID, sessionID string) string {
	return appName + "\x1f" + userID + "\x1f" + sessionID
}
