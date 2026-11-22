package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// status draws a live, single-line progress indicator so long phases never
// look like a hang. On a terminal it redraws in place on stdout; when output
// is redirected it writes a line to stderr every few seconds so stdout stays
// clean and machine-readable.
type status struct {
	out         io.Writer
	interactive bool

	mu         sync.Mutex
	label      string
	detail     string
	lastEvent  string
	phaseStart time.Time
	frame      int
	lastLen    int
	started    bool
	stopped    bool

	stopCh   chan struct{}
	doneCh   chan struct{}
	stopOnce sync.Once
}

func newStatus(stdout io.Writer) *status {
	interactive := false
	out := io.Writer(os.Stderr)
	if f, ok := stdout.(*os.File); ok {
		if info, err := f.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
			interactive = true
			out = f
		}
	}
	return &status{
		out:         out,
		interactive: interactive,
		stopCh:      make(chan struct{}),
		doneCh:      make(chan struct{}),
	}
}

// set changes the current phase and its detail line. The phase timer restarts
// whenever the label changes.
func (s *status) set(label, detail string) {
	s.mu.Lock()
	if label != s.label {
		s.label = label
		s.phaseStart = time.Now()
		// The previous phase's last event no longer describes what is running.
		s.lastEvent = ""
	}
	s.detail = detail
	s.mu.Unlock()
}

// setProgress updates the detail line with done/total counts, the completion
// percentage, the observed rate and an ETA, so a long phase shows measurable
// movement instead of just a number that occasionally changes.
func (s *status) setProgress(label string, done, total int64, extra string) {
	s.mu.Lock()
	start := s.phaseStart
	if label != s.label || start.IsZero() {
		start = time.Now()
	}
	elapsed := time.Since(start)
	s.mu.Unlock()

	detail := fmt.Sprintf("%d/%d", done, total)
	if total > 0 {
		detail += fmt.Sprintf(" (%d%%)", done*100/total)
	}
	if done > 0 && elapsed >= time.Second {
		rate := float64(done) / elapsed.Seconds()
		detail += fmt.Sprintf(", %.1f/s", rate)
		if total > done && rate > 0 {
			eta := time.Duration(float64(total-done)/rate) * time.Second
			detail += ", ETA " + eta.Round(time.Second).String()
		}
	}
	if extra != "" {
		detail += ", " + extra
	}
	s.set(label, detail)
}

// note records the most recent notable event (a source that answered, a working
// proxy) so the progress line shows what is happening right now.
func (s *status) note(event string) {
	s.mu.Lock()
	s.lastEvent = event
	s.mu.Unlock()
}

// LogWriter wraps stderr so each log message clears the progress line first
// instead of being printed on top of it.
func (s *status) LogWriter() io.Writer { return statusLogWriter{s: s} }

type statusLogWriter struct{ s *status }

func (w statusLogWriter) Write(p []byte) (int, error) {
	w.s.clearLine()
	return os.Stderr.Write(p)
}

func (s *status) start() {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.phaseStart = time.Now()
	s.mu.Unlock()

	interval := 5 * time.Second
	if s.interactive {
		interval = 700 * time.Millisecond
	}
	go func() {
		defer close(s.doneCh)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-s.stopCh:
				return
			case <-ticker.C:
				s.render()
			}
		}
	}()
}

func (s *status) render() {
	frames := []string{"-", "\\", "|", "/"}

	s.mu.Lock()
	if s.stopped || s.label == "" {
		s.mu.Unlock()
		return
	}
	frame := frames[s.frame%len(frames)]
	s.frame++
	label, detail, lastEvent := s.label, s.detail, s.lastEvent
	elapsed := time.Since(s.phaseStart).Truncate(time.Second)
	s.mu.Unlock()

	line := frame + " " + label
	if detail != "" {
		line += ": " + detail
	}
	line += " [" + elapsed.String() + "]"
	if lastEvent != "" {
		if r := []rune(lastEvent); len(r) > 60 {
			lastEvent = string(r[:57]) + "..."
		}
		line += " last: " + lastEvent
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.interactive {
		pad := ""
		if extra := s.lastLen - len(line); extra > 0 {
			pad = strings.Repeat(" ", extra)
		}
		fmt.Fprintf(s.out, "\r\033[K%s%s", line, pad)
		s.lastLen = len(line)
	} else {
		fmt.Fprintln(s.out, line)
	}
}

func (s *status) clearLine() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.interactive && s.lastLen > 0 {
		fmt.Fprint(s.out, "\r\033[K")
		s.lastLen = 0
	}
}

// stop ends the indicator and removes its line, leaving the terminal ready for
// normal output. It is safe to call more than once.
func (s *status) stop() {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		started := s.started
		s.mu.Unlock()

		close(s.stopCh)
		if started {
			<-s.doneCh
		}

		s.mu.Lock()
		s.stopped = true
		if s.interactive && s.lastLen > 0 {
			fmt.Fprint(s.out, "\r\033[K")
			s.lastLen = 0
		}
		s.mu.Unlock()
	})
}
