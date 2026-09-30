// Package selflog stores logchipper's own log output as events, so it shows
// up in the UI alongside everything else.
package selflog

import (
	"fmt"
	"io"
	"strings"
	"time"

	"logchipper/internal/db"
	"logchipper/internal/handler"
)

const Source = "logchipper"

// Writer is an io.Writer for log.SetOutput. Each line is copied to Out and
// queued for insertion. Inserts happen on a separate goroutine so a log call
// never blocks on (or deadlocks against) the single DB connection, and
// insert failures go only to Out, never back through the logger.
type Writer struct {
	Out   io.Writer
	queue chan db.Event
}

func New(out io.Writer, database *db.DB, broker *handler.Broker) *Writer {
	w := &Writer{Out: out, queue: make(chan db.Event, 256)}
	go func() {
		for e := range w.queue {
			id, err := database.Insert(e)
			if err != nil {
				fmt.Fprintf(out, "selflog insert error: %v\n", err)
				continue
			}
			e.ID = id
			broker.Publish(e)
		}
	}()
	return w
}

// Write expects the logger's flags to be 0; it adds its own timestamp to Out
// and uses the event's created_at for the stored copy.
func (w *Writer) Write(p []byte) (int, error) {
	now := time.Now()
	fmt.Fprintf(w.Out, "%s %s", now.Format("2006/01/02 15:04:05"), p)

	msg := strings.TrimRight(string(p), "\n")
	select {
	case w.queue <- db.Event{CreatedAt: now, Source: Source, Level: level(msg), Message: msg}:
	default: // queue full: drop the stored copy rather than block the caller
	}
	return len(p), nil
}

func level(msg string) string {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "error"):
		return "error"
	case strings.HasPrefix(m, "warning"):
		return "warn"
	default:
		return "info"
	}
}
