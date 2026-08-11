// session_context_load exercises bounded PostgreSQL conversation and ADK
// session reads without calling an LLM or any voice provider.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"sort"
	"time"

	"xin-chat-robot-agent/internal/conversation"
	conversationcontext "xin-chat-robot-agent/internal/conversation/context"
	conversationpostgres "xin-chat-robot-agent/internal/conversation/postgres"
	"xin-chat-robot-agent/internal/sessionstore"

	"github.com/google/uuid"
	adksession "google.golang.org/adk/v2/session"
)

type durations []time.Duration

type getRequestSpy struct {
	adksession.Service
	maxRecentEvents int
}

func (spy *getRequestSpy) Get(ctx context.Context, request *adksession.GetRequest) (*adksession.GetResponse, error) {
	if request != nil && request.NumRecentEvents > spy.maxRecentEvents {
		spy.maxRecentEvents = request.NumRecentEvents
	}
	return spy.Service.Get(ctx, request)
}

func main() {
	var (
		dsn            string
		appName        string
		userID         string
		conversationID string
		sessionID      string
		seedTurns      int
		measureTurns   int
		maxEvents      int
		maxTurns       int
		recentTurns    int
		textUnits      int
	)
	flag.StringVar(&dsn, "dsn", os.Getenv("ADK_SESSION_POSTGRES_DSN"), "PostgreSQL DSN (defaults to ADK_SESSION_POSTGRES_DSN)")
	flag.StringVar(&appName, "app", "session-context-load", "isolated synthetic app name")
	flag.StringVar(&userID, "user", "load-test-user", "synthetic user ID")
	flag.StringVar(&conversationID, "conversation", "", "synthetic conversation ID (default: timestamped)")
	flag.StringVar(&sessionID, "session", "", "synthetic ADK session ID (default: conversation ID)")
	flag.IntVar(&seedTurns, "seed-turns", 500, "completed turns to seed before measurement")
	flag.IntVar(&measureTurns, "measure-turns", 50, "additional turns and reads to measure")
	flag.IntVar(&maxEvents, "max-events", 48, "maximum ADK events loaded per Get")
	flag.IntVar(&maxTurns, "max-turns", 8, "maximum visible ADK user turns")
	flag.IntVar(&recentTurns, "recent-turns", 12, "maximum semantic conversation turns loaded")
	flag.IntVar(&textUnits, "text-units", 16000, "semantic context text budget")
	flag.Parse()

	if dsn == "" {
		fatalf("--dsn or ADK_SESSION_POSTGRES_DSN is required")
	}
	if seedTurns < 1 || measureTurns < 1 || maxEvents < 1 || maxTurns < 1 || recentTurns < 1 || textUnits < 1 {
		fatalf("all numeric limits must be positive")
	}
	if conversationID == "" {
		conversationID = "run-" + time.Now().UTC().Format("20060102T150405.000000000")
	}
	if sessionID == "" {
		sessionID = conversationID
	}

	ctx := context.Background()
	store, err := conversationpostgres.New(conversationpostgres.Config{DSN: dsn})
	if err != nil {
		fatalf("open conversation store: %v", err)
	}
	reader, err := conversationcontext.New(store, conversationcontext.Config{MaxRecentTurns: recentTurns, MaxTextUnits: textUnits})
	if err != nil {
		fatalf("create context builder: %v", err)
	}
	inner, err := sessionstore.NewPostgresService(sessionstore.PostgresConfig{DSN: dsn})
	if err != nil {
		fatalf("open ADK session store: %v", err)
	}
	innerSpy := &getRequestSpy{Service: inner}
	sessions, err := sessionstore.NewManagedService(innerSpy, sessionstore.Config{MaxEvents: maxEvents, MaxTurns: maxTurns})
	if err != nil {
		fatalf("create managed session store: %v", err)
	}
	created, err := sessions.Create(ctx, &adksession.CreateRequest{AppName: appName, UserID: userID, SessionID: sessionID})
	if err != nil {
		fatalf("create synthetic ADK session: %v", err)
	}
	if created == nil || created.Session == nil {
		fatalf("create synthetic ADK session returned no session")
	}

	key := conversation.ConversationKey{AppName: appName, UserID: userID, ConversationID: conversationID}
	for turn := 1; turn <= seedTurns; turn++ {
		if _, err := writeTurn(ctx, store, key, turn); err != nil {
			fatalf("seed turn %d: %v", turn, err)
		}
		if err := appendADKTurn(ctx, sessions, created.Session); err != nil {
			fatalf("seed ADK turn %d: %v", turn, err)
		}
	}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	beginTimes := make(durations, 0, measureTurns)
	contextTimes := make(durations, 0, measureTurns)
	sessionTimes := make(durations, 0, measureTurns)
	maxLoadedTurns, maxLoadedEvents := 0, 0
	for turn := seedTurns + 1; turn <= seedTurns+measureTurns; turn++ {
		beginDuration, err := writeTurn(ctx, store, key, turn)
		if err != nil {
			fatalf("measured turn %d: %v", turn, err)
		}
		beginTimes = append(beginTimes, beginDuration)

		started := time.Now()
		loaded, err := reader.LoadContext(ctx, key.AppName, key.UserID, key.ConversationID, conversation.ContextLimits{})
		if err != nil {
			fatalf("load context on turn %d: %v", turn, err)
		}
		contextTimes = append(contextTimes, time.Since(started))
		if len(loaded.RecentTurns) > maxLoadedTurns {
			maxLoadedTurns = len(loaded.RecentTurns)
		}

		started = time.Now()
		got, err := sessions.Get(ctx, &adksession.GetRequest{AppName: appName, UserID: userID, SessionID: sessionID})
		if err != nil {
			fatalf("get ADK session on turn %d: %v", turn, err)
		}
		sessionTimes = append(sessionTimes, time.Since(started))
		if got == nil || got.Session == nil {
			fatalf("get ADK session on turn %d returned no session", turn)
		}
		if events := got.Session.Events().Len(); events > maxLoadedEvents {
			maxLoadedEvents = events
		}
		if err := appendADKTurn(ctx, sessions, created.Session); err != nil {
			fatalf("append measured ADK turn %d: %v", turn, err)
		}
	}
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	if maxLoadedTurns > recentTurns {
		fatalf("semantic context loaded %d turns, limit is %d", maxLoadedTurns, recentTurns)
	}
	if maxLoadedEvents > maxEvents {
		fatalf("ADK session exposed %d events, limit is %d", maxLoadedEvents, maxEvents)
	}
	if innerSpy.maxRecentEvents > maxEvents {
		fatalf("ADK session requested %d events, limit is %d", innerSpy.maxRecentEvents, maxEvents)
	}

	fmt.Printf("session-context load report\n")
	fmt.Printf("synthetic_app=%s user=%s conversation=%s session=%s\n", appName, userID, conversationID, sessionID)
	fmt.Printf("seed_turns=%d measured_turns=%d\n", seedTurns, measureTurns)
	printLatency("conversation_begin_turn", beginTimes)
	printLatency("conversation_load_context", contextTimes)
	printLatency("adk_session_get", sessionTimes)
	fmt.Printf("conversation_loaded_turns_max=%d limit=%d\n", maxLoadedTurns, recentTurns)
	fmt.Printf("adk_session_loaded_events_max=%d limit=%d\n", maxLoadedEvents, maxEvents)
	fmt.Printf("adk_session_requested_events_max=%d limit=%d\n", innerSpy.maxRecentEvents, maxEvents)
	fmt.Printf("process_heap_bytes_before=%d after=%d delta=%d\n", before.HeapAlloc, after.HeapAlloc, int64(after.HeapAlloc)-int64(before.HeapAlloc))
	fmt.Printf("result=PASS\n")
}

func writeTurn(ctx context.Context, store conversation.TurnRecorder, key conversation.ConversationKey, turn int) (time.Duration, error) {
	started := time.Now()
	handle, err := store.BeginTurn(ctx, conversation.BeginTurnRequest{
		ConversationKey: key,
		UserText:        fmt.Sprintf("第 %d 轮用户问题：请继续说明当前事项。", turn),
		Source:          "session_context_load",
		IdempotencyKey:  fmt.Sprintf("turn-%d", turn),
	})
	beginDuration := time.Since(started)
	if err != nil {
		return beginDuration, err
	}
	if err := store.FinishTurn(ctx, conversation.FinishTurnRequest{
		Handle:        handle,
		AssistantText: fmt.Sprintf("第 %d 轮助手回答：已记录并保持上下文连续。", turn),
		Status:        conversation.TurnCompleted,
	}); err != nil {
		return beginDuration, err
	}
	return beginDuration, nil
}

func appendADKTurn(ctx context.Context, service *sessionstore.ManagedService, session adksession.Session) error {
	if err := service.AppendEvent(ctx, session, &adksession.Event{ID: uuid.NewString(), Author: "user"}); err != nil {
		return err
	}
	return service.AppendEvent(ctx, session, &adksession.Event{ID: uuid.NewString(), Author: "agent"})
}

func printLatency(name string, values durations) {
	fmt.Printf("%s_ms p50=%.2f p95=%.2f max=%.2f\n", name, values.percentile(50).Seconds()*1000, values.percentile(95).Seconds()*1000, values.percentile(100).Seconds()*1000)
}

func (values durations) percentile(percent int) time.Duration {
	if len(values) == 0 {
		return 0
	}
	ordered := append(durations(nil), values...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	index := (len(ordered)*percent + 99) / 100
	if index < 1 {
		index = 1
	}
	return ordered[index-1]
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "session-context load: "+format+"\n", args...)
	os.Exit(1)
}
