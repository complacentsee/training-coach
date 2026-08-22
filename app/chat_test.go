package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// CHAT_* falls back to GRADER_* for everything but the switch and the
// caps; a set-and-wrong value is fatal, not silently off.
func TestChatConfigFallsBackToTheGraders(t *testing.T) {
	for _, k := range []string{"CHAT_MODE", "CHAT_PROVIDER", "CHAT_DIALECT", "CHAT_MODEL", "CHAT_BASE_URL", "CHAT_API_KEY",
		"CHAT_REASONING_EFFORT", "CHAT_NOTES_MAX", "CHAT_TURNS_PER_DAY", "GRADER_PROVIDER", "GRADER_DIALECT",
		"GRADER_MODEL", "GRADER_BASE_URL", "GRADER_API_KEY", "GRADER_REASONING_EFFORT"} {
		t.Setenv(k, "")
	}
	c, err := chatConfigFromEnv()
	if err != nil || c.Mode != "off" {
		t.Fatalf("default: %+v %v, want off", c, err)
	}
	t.Setenv("CHAT_MODE", "on")
	t.Setenv("GRADER_PROVIDER", "openai")
	t.Setenv("GRADER_MODEL", "gpt-x")
	t.Setenv("GRADER_BASE_URL", "http://grader.example/v1")
	t.Setenv("GRADER_API_KEY", "k")
	c, err = chatConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.Provider != "openai" || c.Dialect != "openai" || c.Model != "gpt-x" || c.BaseURL != "http://grader.example/v1" || c.Key != "k" {
		t.Errorf("fallback: %+v", c)
	}
	if c.TurnsPerDay != defaultChatTurns || c.NotesMax != defaultNotesMax {
		t.Errorf("defaults: turns %d notes %d", c.TurnsPerDay, c.NotesMax)
	}
	t.Setenv("CHAT_MODEL", "claude-x")
	t.Setenv("CHAT_PROVIDER", "anthropic")
	t.Setenv("CHAT_BASE_URL", "")
	t.Setenv("CHAT_TURNS_PER_DAY", "5")
	c, err = chatConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.Provider != "anthropic" || c.Dialect != "anthropic" || c.Model != "claude-x" || c.BaseURL != "http://grader.example/v1" || c.TurnsPerDay != 5 {
		t.Errorf("override: %+v", c)
	}
	for _, bad := range [][2]string{{"CHAT_MODE", "maybe"}, {"CHAT_TURNS_PER_DAY", "0"}, {"CHAT_NOTES_MAX", "lots"}, {"CHAT_PROVIDER", "other"}, {"CHAT_DIALECT", "grpc"}} {
		t.Setenv("CHAT_MODE", "on")
		t.Setenv(bad[0], bad[1])
		if _, err := chatConfigFromEnv(); err == nil {
			t.Errorf("%s=%s accepted", bad[0], bad[1])
		}
		t.Setenv(bad[0], "")
	}
	t.Setenv("CHAT_MODE", "on")
	t.Setenv("CHAT_PROVIDER", "openai")
	t.Setenv("CHAT_MODEL", "")
	t.Setenv("GRADER_MODEL", "")
	if _, err := chatConfigFromEnv(); err == nil {
		t.Error("openai with no model anywhere accepted")
	}
}

// The transcript file is append-only, survives a restart, and a line
// the reader cannot parse costs what follows it in that read but never
// the file.
func TestChatStoreAppendsAndReplays(t *testing.T) {
	dir := t.TempDir()
	cs, err := openChatStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	lines := []chatLine{
		{Date: "2026-01-13", Role: "user", Text: "calf at 4, what gives?"},
		{Date: "2026-01-13", Role: "assistant", Text: "*Hold* the hills."},
		{Date: "2026-01-12", Role: "user", Text: "yesterday"},
	}
	for _, l := range lines {
		if err := cs.Append(l); err != nil {
			t.Fatal(err)
		}
	}
	if got := cs.Turns("2026-01-13"); got != 1 {
		t.Errorf("turns: %d, want 1 (only the athlete's messages count)", got)
	}
	if got := cs.Dates(); len(got) != 2 || got[0] != "2026-01-13" {
		t.Errorf("dates newest first: %v", got)
	}
	raw := string(readOrFail(t, filepath.Join(dir, "chat", "2026-01-13.jsonl")))
	if strings.Count(raw, "\n") != 2 || strings.Count(string(readOrFail(t, filepath.Join(dir, "chat", "2026-01-12.jsonl"))), "\n") != 1 {
		t.Errorf("each day its own file; 13 Jan holds:\n%s", raw)
	}
	again, err := openChatStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	day := again.Day("2026-01-13")
	if len(day) != 2 || day[0].Text != lines[0].Text || day[1].Text != lines[1].Text || day[0].TS.IsZero() {
		t.Errorf("replay: %+v", day)
	}
	if err := again.Append(chatLine{Date: "2026-01-13", Role: "user", Text: "more"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(readOrFail(t, filepath.Join(dir, "chat", "2026-01-13.jsonl"))), raw) {
		t.Error("an append rewrote what was there")
	}
	// A torn line — a crash between write and sync — costs that line only.
	f, err := os.OpenFile(filepath.Join(dir, "chat", "2026-01-13.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"ts":"2026-01-13T08:00:00Z","date":"2026-01-13","ro` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := again.Append(chatLine{Date: "2026-01-13", Role: "assistant", Text: "after the tear"}); err != nil {
		t.Fatal(err)
	}
	third, err := openChatStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if day := third.Day("2026-01-13"); len(day) != 4 || day[3].Text != "after the tear" {
		t.Errorf("a torn line hid what followed it: %+v", day)
	}
	// Retention: keeping from the 13th deletes the 12th's file whole and
	// forgets it; a stranger in the directory is not this code's to remove.
	if err := os.WriteFile(filepath.Join(dir, "chat", "notes.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := third.Prune("2026-01-13"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "chat", "2026-01-12.jsonl")); !os.IsNotExist(err) {
		t.Error("the 12th's file should have been deleted")
	}
	if len(third.Day("2026-01-12")) != 0 || len(third.Day("2026-01-13")) != 4 || len(third.Dates()) != 1 {
		t.Errorf("after the prune: 12th %d lines, 13th %d, dates %v", len(third.Day("2026-01-12")), len(third.Day("2026-01-13")), third.Dates())
	}
	if _, err := os.Stat(filepath.Join(dir, "chat", "notes.txt")); err != nil {
		t.Error("the prune removed a file it did not write")
	}
}

// A chat.jsonl from before the per-day files is read once, its lines land
// in their days' files, and the old file is renamed aside, never deleted.
func TestChatStoreMigratesTheSingleFile(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"ts":"2026-01-12T20:00:00Z","date":"2026-01-12","role":"user","text":"old"}
{"ts":"2026-01-13T20:00:00Z","date":"2026-01-13","role":"user","text":"newer"}
{"ts":"2026-01-13T20:00:05Z","date":"2026-01-13","role":"assistant","text":"reply"}
`
	if err := os.WriteFile(filepath.Join(dir, "chat.jsonl"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	cs, err := openChatStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.Day("2026-01-13")) != 2 || len(cs.Day("2026-01-12")) != 1 {
		t.Errorf("migrated days: %v", cs.Dates())
	}
	if _, err := os.Stat(filepath.Join(dir, "chat.jsonl")); !os.IsNotExist(err) {
		t.Error("chat.jsonl should have been renamed aside")
	}
	if string(readOrFail(t, filepath.Join(dir, "chat.jsonl.migrated"))) != legacy {
		t.Error("the retired file is not byte-identical to the original")
	}
	if !strings.Contains(string(readOrFail(t, filepath.Join(dir, "chat", "2026-01-13.jsonl"))), `"text":"reply"`) {
		t.Error("the 13th's file lacks the migrated reply")
	}
	// A second open finds only day files: the migration ran once.
	again, err := openChatStore(dir)
	if err != nil || len(again.Day("2026-01-13")) != 2 {
		t.Errorf("second open: %v, 13th has %d lines", err, len(again.Day("2026-01-13")))
	}
}

// The coach keeps today and yesterday: its prune, run at startup and at
// the daily tick, drops the day before.
func TestCoachPruneKeepsTodayAndYesterday(t *testing.T) {
	_, c, _ := coachUnderTest(t)
	for _, d := range []string{"2026-01-11", "2026-01-12", "2026-01-13"} {
		if err := c.store.Append(chatLine{Date: d, Role: "user", Text: d}); err != nil {
			t.Fatal(err)
		}
	}
	c.prune()
	if got := c.store.Dates(); len(got) != 2 || got[0] != "2026-01-13" || got[1] != "2026-01-12" {
		t.Errorf("kept %v, want today and yesterday", got)
	}
}

// A fake model: answers with one tool call, then with prose that quotes
// the tool's result, so the test can see the loop carried the result
// back. Records what it was sent.
type fakeTurn struct {
	mu      sync.Mutex
	systems []string
	msgs    [][]llmMsg
	block   chan struct{} // when non-nil, the first call waits on it
}

func (f *fakeTurn) turn(_ context.Context, system string, msgs []llmMsg, tools []llmTool) (llmMsg, string, error) {
	f.mu.Lock()
	f.systems = append(f.systems, system)
	cp := make([]llmMsg, len(msgs))
	copy(cp, msgs)
	f.msgs = append(f.msgs, cp)
	first := len(f.msgs) == 1
	f.mu.Unlock()
	if first && f.block != nil {
		<-f.block
	}
	last := msgs[len(msgs)-1]
	if last.Role == "tool" {
		return llmMsg{Role: "assistant", Text: "The week holds " + last.Results[0].Content[:20] + "… so *absorb it*."}, "end_turn", nil
	}
	// Folded history joins consecutive user turns, so key on the latest
	// line rather than any substring.
	if lines := strings.Split(strings.TrimSpace(last.Text), "\n"); strings.HasSuffix(lines[len(lines)-1], "fail") {
		return llmMsg{}, "", context.DeadlineExceeded
	}
	return llmMsg{Role: "assistant", Calls: []llmToolCall{{ID: "c1", Name: "get_week",
		Args: json.RawMessage(`{"date":"2026-01-13"}`)}}}, "tool_use", nil
}

func coachUnderTest(t *testing.T) (testServer, *coach, *fakeTurn) {
	t.Helper()
	dir := t.TempDir()
	ts := fitTestMuxServer(t, dir)
	cs, err := openChatStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := newCoach(ts.s, chatConfig{Mode: "on", Provider: "anthropic", Dialect: "anthropic",
		Model: "m", BaseURL: "http://unused.invalid", TurnsPerDay: 3}, cs)
	f := &fakeTurn{}
	c.turn = f.turn
	c.today = func() time.Time { return time.Date(2026, 1, 13, 0, 0, 0, 0, time.UTC) }
	c.now = func() time.Time { return time.Date(2026, 1, 13, 21, 40, 0, 0, time.UTC) }
	ts.s.coach = c
	return ts, c, f
}

func waitIdle(t *testing.T, c *coach, date string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		c.mu.Lock()
		busy := c.busy[date]
		c.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("turn never finished")
}

// POST → 202, the message is in the transcript before the model runs,
// the model's tool call is served from the app's own payloads, and the
// reply lands in the transcript the page polls.
func TestChatTurnRunsToolsAndRecordsTheReply(t *testing.T) {
	ts, c, f := coachUnderTest(t)
	rec := httptest.NewRecorder()
	ts.mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"text":"calf at 4, what gives this week?"}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST: %d %s", rec.Code, rec.Body)
	}
	waitIdle(t, c, "2026-01-13")

	rec = httptest.NewRecorder()
	ts.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/chat", nil))
	var out struct {
		Date     string `json:"date"`
		Busy     bool   `json:"busy"`
		Turns    int    `json:"turns"`
		Cap      int    `json:"cap"`
		Messages []struct{ Role, Text string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Date != "2026-01-13" || out.Busy || out.Turns != 1 || out.Cap != 3 {
		t.Errorf("state: %+v", out)
	}
	if len(out.Messages) != 2 || out.Messages[0].Role != "user" || out.Messages[1].Role != "assistant" ||
		!strings.Contains(out.Messages[1].Text, "absorb it") {
		t.Fatalf("transcript: %+v", out.Messages)
	}
	// The model was sent the chat's own procedure with the derived
	// context, never the grading procedure.
	if len(f.systems) != 2 || !strings.Contains(f.systems[0], "You change nothing") ||
		!strings.Contains(f.systems[0], "Today is Tue 13 Jan 2026 and it is 9:40 pm") ||
		!strings.Contains(f.systems[0], "The day is nearly over") || strings.Contains(f.systems[0], gradingProcedure[:40]) {
		t.Errorf("system prompt: %q", f.systems[0][:200])
	}
	// At noon the evening line is absent; the hour is still stated.
	c.now = func() time.Time { return time.Date(2026, 1, 13, 12, 5, 0, 0, time.UTC) }
	if sp := c.systemPrompt("2026-01-13"); !strings.Contains(sp, "it is 12:05 pm") || strings.Contains(sp, "nearly over") {
		t.Errorf("midday context: %q", sp[len(sp)-400:])
	}
	// The tool result it saw was get_week's JSON for the defaults block.
	toolMsg := f.msgs[1][len(f.msgs[1])-1]
	if toolMsg.Role != "tool" || len(toolMsg.Results) != 1 || toolMsg.Results[0].IsError ||
		!strings.Contains(toolMsg.Results[0].Content, `"week":2`) || !strings.Contains(toolMsg.Results[0].Content, `"date":"2026-01-13"`) {
		t.Errorf("tool result: %+v", toolMsg)
	}
	// And it is on disk.
	raw := readOrFail(t, filepath.Join(ts.s.dataDir, "chat", "2026-01-13.jsonl"))
	if strings.Count(string(raw), "\n") != 2 {
		t.Errorf("the day's file:\n%s", raw)
	}
}

// One turn in flight per day; a second message is refused, not queued.
// The cap counts the athlete's messages; a failed turn is recorded as
// such and the next message still goes.
func TestChatSingleFlightCapAndFailure(t *testing.T) {
	ts, c, f := coachUnderTest(t)
	f.block = make(chan struct{})
	post := func(text string) int {
		rec := httptest.NewRecorder()
		ts.mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"text":"`+text+`"}`)))
		return rec.Code
	}
	if code := post("first"); code != http.StatusAccepted {
		t.Fatalf("first: %d", code)
	}
	if code := post("second while busy"); code != http.StatusConflict {
		t.Errorf("second while busy: %d, want 409", code)
	}
	close(f.block)
	waitIdle(t, c, "2026-01-13")

	if code := post("please fail"); code != http.StatusAccepted {
		t.Fatalf("failing turn: %d", code)
	}
	waitIdle(t, c, "2026-01-13")
	day := c.store.Day("2026-01-13")
	if len(day) != 4 || day[3].Role != "error" || !strings.Contains(day[3].Text, "did not complete") {
		t.Errorf("after a failed turn: %+v", day)
	}
	// The failed turn is not charged: with a cap of 3, two answered
	// messages leave room for one more, and only then is the day closed.
	if code := post("third"); code != http.StatusAccepted {
		t.Fatalf("third: %d", code)
	}
	waitIdle(t, c, "2026-01-13")
	if got := c.store.Turns("2026-01-13"); got != 2 {
		t.Errorf("turns charged: %d, want 2 (the failed one is free)", got)
	}
	if code := post("fourth"); code != http.StatusAccepted {
		t.Fatalf("fourth: %d", code)
	}
	waitIdle(t, c, "2026-01-13")
	if code := post("fifth, over the cap"); code != http.StatusTooManyRequests {
		t.Errorf("over the cap: %d, want 429", code)
	}
	// Yesterday's conversation is a record, not a place to talk.
	rec := httptest.NewRecorder()
	ts.mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"date":"2026-01-12","text":"late"}`)))
	if rec.Code != http.StatusConflict {
		t.Errorf("message to an earlier day: %d, want 409", rec.Code)
	}
	if code := post(strings.Repeat("é", chatTextMax+1)); code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize: %d, want 413 — the cap is characters, the textarea's unit", code)
	}
	rec = httptest.NewRecorder()
	ts.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/coach?date=2026-8-5", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("/coach with a malformed date: %d, want 400, never a silent fallback to today", rec.Code)
	}
}

// Off: the page says so, the API refuses, and the nav offers nothing.
func TestChatOffIsVisiblyOff(t *testing.T) {
	ts := fitTestMuxServer(t, t.TempDir())
	for _, p := range []string{"/api/chat"} {
		rec := httptest.NewRecorder()
		ts.mux.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("GET %s: %d, want 503", p, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	ts.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/coach", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "switched off") {
		t.Errorf("/coach when off: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	ts.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if strings.Contains(rec.Body.String(), `href="/coach"`) {
		t.Error("the nav offers Coach with no coach configured")
	}
	ts2, _, _ := coachUnderTest(t)
	rec = httptest.NewRecorder()
	ts2.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if !strings.Contains(rec.Body.String(), `href="/coach"`) {
		t.Error("the nav does not offer Coach when one is configured")
	}
	rec = httptest.NewRecorder()
	ts2.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/coach", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `id="say"`) || !strings.Contains(rec.Body.String(), "/static/coach.") {
		t.Errorf("/coach page: %d, composer=%v", rec.Code, strings.Contains(rec.Body.String(), `id="say"`))
	}
}

// The read tools answer from the app's own payloads and refuse bad input.
func TestChatToolsAreReadsOfTheAppsOwnPayloads(t *testing.T) {
	ts, c, _ := coachUnderTest(t)
	tools := map[string]llmTool{}
	for _, tl := range c.tools("2026-01-13") {
		tools[tl.Name] = tl
	}
	for _, name := range []string{"get_prescription", "get_week", "session_history", "get_metrics", "get_recent_entries", "get_rework", "get_issue_adherence", "get_trends"} {
		if _, ok := tools[name]; !ok {
			t.Errorf("tool %s missing", name)
		}
	}
	ctx := context.Background()
	out, err := tools["get_week"].Run(ctx, json.RawMessage(`{"date":"2026-01-13"}`))
	if err != nil || !strings.Contains(out, `"prescribed_volume"`) || strings.Count(out, `"date":"2026-01-`) != 7 {
		t.Errorf("get_week: %v %s", err, out)
	}
	if _, err := tools["get_week"].Run(ctx, json.RawMessage(`{"date":"2030-01-01"}`)); err == nil {
		t.Error("get_week outside every block should refuse")
	}
	out, err = tools["get_rework"].Run(ctx, json.RawMessage(`{"date":"2026-01-13"}`))
	if err != nil || !strings.Contains(out, `"candidates"`) && !strings.Contains(out, `"reason"`) {
		t.Errorf("get_rework: %v %s", err, out)
	}
	if _, err := tools["get_rework"].Run(ctx, json.RawMessage(`{"date":"../etc"}`)); err == nil {
		t.Error("get_rework with a bad date should refuse")
	}
	// Adherence is the card's ledger for TODAY, so it is asked of a block
	// that holds today: the defaults block shifted around the clock.
	{
		dir := t.TempDir()
		shiftedBlock(t, dir)
		ts3 := fitTestMuxServer(t, dir)
		out, err := ts3.s.internalGET("/api/issue-adherence?key=achilles")
		if err != nil || !strings.Contains(out, "achilles") {
			t.Errorf("get_issue_adherence through the app's own route: %v %s", err, out)
		}
		if _, err := ts3.s.internalGET("/api/issue-adherence?key=nothing"); err == nil || !strings.Contains(err.Error(), "no issue") {
			t.Errorf("an unknown issue should refuse with the handler's reason, got %v", err)
		}
	}
	if _, err := tools["get_issue_adherence"].Run(ctx, json.RawMessage(`{"key":"no such"}`)); err == nil {
		t.Error("a key with a space should refuse")
	}
	out, err = tools["get_trends"].Run(ctx, json.RawMessage(`{}`))
	if err != nil || out != "[]" {
		t.Errorf("get_trends on an unmeasured block: %v %q", err, out)
	}
	if err := ts.s.store.Append(Entry{Date: "2026-01-13", Kind: "note", Note: "slept badly"}); err != nil {
		t.Fatal(err)
	}
	out, err = tools["get_recent_entries"].Run(ctx, json.RawMessage(`{"days":3}`))
	if err != nil || !strings.Contains(out, "slept badly") {
		t.Errorf("get_recent_entries: %v %s", err, out)
	}
	// A coaching-notes file joins the system prompt; the grading notes do not.
	if err := os.WriteFile(filepath.Join(ts.s.dataDir, "coaching-notes.md"), []byte("Never suggest doubles.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if sp := c.systemPrompt("2026-01-13"); !strings.Contains(sp, "Never suggest doubles") || !strings.Contains(sp, "Messages used today: 0 of 3") {
		t.Errorf("system prompt lacks the notes or the context:\n%s", sp)
	}
}

// Phase 2b. The model may propose exactly what the rework flow offers for
// a date, once per reply; the proposal is a card in the transcript; the
// athlete's decision is recorded against it; nothing in the plan moves
// until /api/amend is called by the page.
func TestProposeAmendmentIsBoundedByTheReworkFlow(t *testing.T) {
	dir := t.TempDir()
	start := shiftedBlock(t, dir) // week 3 is ahead: Tuesday quality, Thursday rest
	ts := fitTestMuxServer(t, dir)
	cs, err := openChatStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := newCoach(ts.s, chatConfig{Mode: "on", Provider: "anthropic", Dialect: "anthropic", Model: "m",
		BaseURL: "http://unused.invalid", TurnsPerDay: 10}, cs)
	ts.s.coach = c
	today := ts.s.day(ts.s.ds()).Format("2006-01-02")
	c.today = func() time.Time { return ts.s.day(ts.s.ds()) }
	tools := map[string]llmTool{}
	for _, tl := range c.tools(today) {
		tools[tl.Name] = tl
	}
	propose := tools["propose_amendment"]
	if propose.Run == nil {
		t.Fatal("no propose_amendment tool")
	}
	tue := start.AddDate(0, 0, 15).Format("2006-01-02") // week 3 Tuesday
	thu := start.AddDate(0, 0, 17).Format("2006-01-02")
	ctx := context.Background()

	// Not a candidate: a move onto Wednesday's easy run.
	wed := start.AddDate(0, 0, 16).Format("2006-01-02")
	if _, err := propose.Run(ctx, json.RawMessage(`{"date":"`+tue+`","op":"move","arg":"`+wed+`","reason":"x"}`)); err == nil || !strings.Contains(err.Error(), "not one of the candidates") {
		t.Errorf("a move the flow does not offer was accepted: %v", err)
	}
	// The candidate the flow offers: Tuesday's quality to Thursday's rest.
	out, err := propose.Run(ctx, json.RawMessage(`{"date":"`+tue+`","op":"move","arg":"`+thu+`","reason":"the calf wants two more days"}`))
	if err != nil || !strings.Contains(out, "Proposed") || !strings.Contains(out, "not applied") {
		t.Fatalf("proposing the offered move: %v %q", err, out)
	}
	// Once per reply.
	if _, err := propose.Run(ctx, json.RawMessage(`{"date":"`+tue+`","op":"cancel","reason":"y"}`)); err == nil || !strings.Contains(err.Error(), "one proposal per reply") {
		t.Errorf("a second proposal in one reply was accepted: %v", err)
	}
	// The card is in the transcript with its structured half.
	day := cs.Day(today)
	if len(day) != 1 || day[0].Role != "proposal" {
		t.Fatalf("transcript: %+v", day)
	}
	var p chatProposal
	if err := json.Unmarshal(day[0].Data, &p); err != nil || p.Date != tue || p.Op != "move" || p.Arg != thu || p.Reason != "the calf wants two more days" || !strings.HasPrefix(p.Title, "Move to ") {
		t.Errorf("proposal data: %+v %v", p, err)
	}
	// Nothing moved.
	if e := ts.s.effective(); e.applied != 0 {
		t.Errorf("a proposal applied an amendment by itself: %d standing", e.applied)
	}
	// A graded day cannot be proposed against at all.
	if err := ts.s.store.Append(Entry{Date: tue, Kind: "grade", Val: "A"}); err != nil {
		t.Fatal(err)
	}
	for _, tl := range c.tools(today) {
		if tl.Name == "propose_amendment" {
			if _, err := tl.Run(ctx, json.RawMessage(`{"date":"`+tue+`","op":"move","arg":"`+thu+`"}`)); err == nil || !strings.Contains(err.Error(), "refuses") {
				t.Errorf("a graded day was proposable: %v", err)
			}
		}
	}

	// The page: GET carries the proposal with its id and data; a decision
	// records against it once; a second is a 409; an unknown id a 404.
	rec := httptest.NewRecorder()
	ts.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/chat", nil))
	var got struct {
		Messages []struct {
			ID   string          `json:"id"`
			Role string          `json:"role"`
			Data json.RawMessage `json:"data"`
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Messages) != 1 || got.Messages[0].Role != "proposal" || got.Messages[0].ID == "" || len(got.Messages[0].Data) == 0 {
		t.Fatalf("GET /api/chat: %v %s", err, rec.Body.String())
	}
	id := got.Messages[0].ID
	post := func(body string) int {
		rec := httptest.NewRecorder()
		ts.mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/chat/decide", strings.NewReader(body)))
		return rec.Code
	}
	if code := post(`{"date":"` + today + `","proposal":"nope","status":"applied"}`); code != http.StatusNotFound {
		t.Errorf("unknown proposal: %d", code)
	}
	if code := post(`{"date":"` + today + `","proposal":"` + id + `","status":"maybe"}`); code != http.StatusBadRequest {
		t.Errorf("bad status: %d", code)
	}
	if code := post(`{"date":"` + today + `","proposal":"` + id + `","status":"dismissed"}`); code != http.StatusOK {
		t.Errorf("dismiss: %d", code)
	}
	if code := post(`{"date":"` + today + `","proposal":"` + id + `","status":"applied"}`); code != http.StatusConflict {
		t.Errorf("a second decision: %d, want 409", code)
	}
	// The model sees its proposal and the athlete's answer next turn, as
	// alternating turns.
	h := c.history(today)
	if len(h) != 2 || h[0].Role != "assistant" || !strings.Contains(h[0].Text, "[proposed: Move to ") || h[1].Role != "user" || !strings.Contains(h[1].Text, "dismissed") {
		t.Errorf("history: %+v", h)
	}
}
