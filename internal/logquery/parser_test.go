package logquery

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestParseTextLine(t *testing.T) {
	loc := time.UTC
	line := "2026-11-21 10:25:31.125 ERROR [http-nio-9001-exec-1] c.e.Service - Failed to process request"
	p := parseLine(line, loc)
	if p.Timestamp == nil {
		t.Fatal("timestamp was not parsed")
	}
	if got := p.Timestamp.Format("2006-01-02 15:04:05.000"); got != "2026-11-21 10:25:31.125" {
		t.Fatalf("timestamp = %s", got)
	}
	if p.Level != "ERROR" {
		t.Fatalf("level = %q", p.Level)
	}
	if p.Message != line {
		t.Fatalf("message = %q", p.Message)
	}
}

func TestParseRFC3339Level(t *testing.T) {
	p := parseLine("2026-11-21T10:25:31.125+08:00 WARN something happened", time.UTC)
	if p.Timestamp == nil || p.Level != "WARN" {
		t.Fatalf("parsed = %#v", p)
	}
	if p.Timestamp.UTC().Hour() != 2 {
		t.Fatalf("timezone not applied: %s", p.Timestamp)
	}
}

func TestParseJSONLine(t *testing.T) {
	line := `{"timestamp":"2026-11-21T10:25:31.125+08:00","level":"ERROR","message":"boom","traceId":"85300926","spanId":"abc12345","stacktrace":["at a.b.C.d(C.java:1)"]}`
	p := parseLine(line, time.UTC)
	if !p.IsJSON || p.Level != "ERROR" || p.Message != "boom" {
		t.Fatalf("parsed = %#v", p)
	}
	if p.TraceID != "85300926" || p.SpanID != "abc12345" {
		t.Fatalf("ids = %#v", p)
	}
	if len(p.Stack) != 1 {
		t.Fatalf("stack = %#v", p.Stack)
	}
}

func TestParseTraceFields(t *testing.T) {
	p := parseLine("2026-11-21 10:25:31 INFO [traceId=85300926,spanId=abc12345,requestId=req-1] done", time.UTC)
	if p.TraceID != "85300926" {
		t.Fatalf("trace id = %q", p.TraceID)
	}
	if p.SpanID != "abc12345" {
		t.Fatalf("span id = %q", p.SpanID)
	}
	if p.RequestID != "req-1" {
		t.Fatalf("request id = %q", p.RequestID)
	}
}

func TestParseBracketMDCTrace(t *testing.T) {
	line := "2026-11-21T14:28:17.208+08:00 INFO  [http-nio-8670-exec-7] [8b5cff7b1d4a-c99d878693fed953] c.a.w.a.a.w.l.WebLog"
	p := parseLine(line, time.UTC)
	if p.TraceID != "8b5cff7b1d4a" {
		t.Fatalf("trace id = %q", p.TraceID)
	}
	if p.SpanID != "c99d878693fed953" {
		t.Fatalf("span id = %q", p.SpanID)
	}
	if p.Level != "INFO" {
		t.Fatalf("level = %q", p.Level)
	}
}

func TestMultilineJavaEvent(t *testing.T) {
	content := `2026-11-21 10:25:31.125 ERROR Failed to process request
java.lang.RuntimeException: Request failed
    at com.example.Service.execute(Service.java:31)
    at com.example.Controller.handle(Controller.java:21)
Caused by: java.lang.NullPointerException
    at com.example.Repository.query(Repository.java:18)
    ... 12 common frames omitted
2026-11-21 10:25:32.000 INFO recovered
`
	f, size := writeTempFile(t, "java.log", content)
	budget := newScanBudget(1<<20, time.Minute)
	fr := newForwardReader(context.Background(), f, 0, size, newTextDecoder("UTF-8"), 32, 1<<16, budget)
	scanner := newForwardEventScanner(fr, time.UTC, 128<<10)

	first, err := scanner.Next()
	if err != nil {
		t.Fatal(err)
	}
	if first.Level != "ERROR" {
		t.Fatalf("level = %q", first.Level)
	}
	if len(first.Lines) != 7 {
		t.Fatalf("event lines = %d: %#v", len(first.Lines), first.Lines)
	}
	if first.Exception != "java.lang.RuntimeException" {
		t.Fatalf("exception = %q", first.Exception)
	}
	if !strings.Contains(first.MessageText(true), "Caused by: java.lang.NullPointerException") {
		t.Fatalf("message = %q", first.MessageText(true))
	}

	second, err := scanner.Next()
	if err != nil {
		t.Fatal(err)
	}
	if second.Level != "INFO" || len(second.Lines) != 1 {
		t.Fatalf("second event = %#v", second)
	}
}

func TestReverseMultilineEvent(t *testing.T) {
	content := `2026-11-21 10:25:31.125 ERROR Failed to process request
java.lang.RuntimeException: Request failed
    at com.example.Service.execute(Service.java:31)
Caused by: java.lang.NullPointerException
2026-11-21 10:25:32.000 INFO recovered
`
	f, size := writeTempFile(t, "java.log", content)
	budget := newScanBudget(1<<20, time.Minute)
	rr := newReverseReader(context.Background(), f, size, newTextDecoder("UTF-8"), 16, 1<<16, budget)
	scanner := newReverseEventScanner(rr, time.UTC, 128<<10)

	first, err := scanner.Next()
	if err != nil {
		t.Fatal(err)
	}
	if first.Level != "INFO" || len(first.Lines) != 1 {
		t.Fatalf("newest event = %#v", first)
	}
	second, err := scanner.Next()
	if err != nil {
		t.Fatal(err)
	}
	if second.Level != "ERROR" || len(second.Lines) != 4 {
		t.Fatalf("older event = %#v", second)
	}
}

func TestFingerprintNormalization(t *testing.T) {
	ev1 := &Event{Message: "Request 123 failed at 2026-11-21T10:25:31Z from 10.0.0.1", Level: "ERROR",
		Stack: []string{"at a.b.C.d(C.java:12)"}}
	ev2 := &Event{Message: "Request 456 failed at 2026-11-21T11:00:00Z from 10.0.0.2", Level: "ERROR",
		Stack: []string{"at a.b.C.d(C.java:99)"}}
	fp1, _ := fingerprintEvent(ev1)
	fp2, _ := fingerprintEvent(ev2)
	if fp1 != fp2 {
		t.Fatalf("fingerprints differ: %s vs %s", fp1, fp2)
	}

	ev3 := &Event{Message: "Something entirely different", Level: "ERROR"}
	fp3, _ := fingerprintEvent(ev3)
	if fp3 == fp1 {
		t.Fatalf("unrelated errors merged into %s", fp3)
	}
}
