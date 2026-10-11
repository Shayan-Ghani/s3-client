package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	statePending int32 = iota
	stateActive
	stateDone
	stateFailed
)

// tracker records transfer progress of one file.
//
// Progress is recorded per chunk as a high-water mark, so when a chunk is
// retried (common on flaky connections) its bytes are not counted twice.
type tracker struct {
	name  string
	total int64

	sent  int64 // atomic
	state int32 // atomic

	mu    sync.Mutex
	parts map[int64]int64
}

func newTracker(name string, total int64) *tracker {
	return &tracker{name: name, total: total, parts: map[int64]int64{}}
}

// record notes that `n` bytes of chunk `key` have been transferred in the
// current attempt.
func (t *tracker) record(key, n int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if n > t.parts[key] {
		atomic.AddInt64(&t.sent, n-t.parts[key])
		t.parts[key] = n
	}
}

func (t *tracker) bytes() int64 {
	v := atomic.LoadInt64(&t.sent)
	if v > t.total {
		v = t.total
	}
	return v
}

func (t *tracker) setState(s int32) { atomic.StoreInt32(&t.state, s) }
func (t *tracker) getState() int32  { return atomic.LoadInt32(&t.state) }

func (t *tracker) complete() {
	atomic.StoreInt64(&t.sent, t.total)
	t.setState(stateDone)
}

func (t *tracker) percent() float64 {
	if t.total == 0 {
		if t.getState() == stateDone {
			return 100
		}
		return 0
	}
	return float64(t.bytes()) * 100 / float64(t.total)
}

// reporter renders overall progress to the terminal.
// On a TTY it redraws one line; otherwise it prints a line every few seconds
// so that logs stay readable.
type reporter struct {
	out      *os.File
	tty      bool
	trackers []*tracker
	start    time.Time

	mu       sync.Mutex
	lastLen  int
	lastSent int64
	lastTick time.Time
	speed    float64 // bytes/s, smoothed

	stop chan struct{}
	done chan struct{}
}

func newReporter(out *os.File, trackers []*tracker) *reporter {
	tty := false
	if st, err := out.Stat(); err == nil {
		tty = st.Mode()&os.ModeCharDevice != 0
	}
	return &reporter{
		out:      out,
		tty:      tty,
		trackers: trackers,
		start:    time.Now(),
		lastTick: time.Now(),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

func (r *reporter) Start() {
	interval := 500 * time.Millisecond
	if !r.tty {
		interval = 10 * time.Second
	}
	go func() {
		defer close(r.done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				r.render()
			case <-r.stop:
				return
			}
		}
	}()
}

// Stop halts rendering and erases the progress line.
func (r *reporter) Stop() {
	close(r.stop)
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clearLocked()
}

// Logf prints a message without garbling the progress line.
func (r *reporter) Logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clearLocked()
	fmt.Fprintf(r.out, format+"\n", args...)
}

func (r *reporter) clearLocked() {
	if r.tty && r.lastLen > 0 {
		fmt.Fprintf(r.out, "\r%s\r", strings.Repeat(" ", r.lastLen))
		r.lastLen = 0
	}
}

func (r *reporter) render() {
	r.mu.Lock()
	defer r.mu.Unlock()

	var sent, total int64
	var finished int
	var active []*tracker
	for _, t := range r.trackers {
		switch t.getState() {
		case stateFailed:
			finished++
			continue // excluded from totals
		case stateDone:
			finished++
		case stateActive:
			active = append(active, t)
		}
		sent += t.bytes()
		total += t.total
	}

	now := time.Now()
	if dt := now.Sub(r.lastTick).Seconds(); dt >= 0.1 {
		inst := float64(sent-r.lastSent) / dt
		if inst < 0 {
			inst = 0
		}
		if r.speed == 0 {
			r.speed = inst
		} else {
			r.speed = 0.3*inst + 0.7*r.speed
		}
		r.lastSent, r.lastTick = sent, now
	}

	eta := "--"
	if r.speed > 1 && total > sent {
		eta = fmtDuration(time.Duration(float64(total-sent) / r.speed * float64(time.Second)))
	}

	var line string
	if len(r.trackers) == 1 {
		t := r.trackers[0]
		line = fmt.Sprintf("%s %5.1f%% | %s / %s | %s/s | ETA %s",
			shorten(t.name, 30), t.percent(), fmtBytes(sent), fmtBytes(total), fmtBytes(int64(r.speed)), eta)
	} else {
		name, extra := "waiting...", ""
		pct := 0.0
		if len(active) > 0 {
			name, pct = shorten(active[0].name, 24), active[0].percent()
			if len(active) > 1 {
				extra = fmt.Sprintf(" (+%d more)", len(active)-1)
			}
		}
		overall := 100.0
		if total > 0 {
			overall = float64(sent) * 100 / float64(total)
		}
		line = fmt.Sprintf("[%d/%d] %s %5.1f%%%s | total %s / %s %.1f%% | %s/s | ETA %s",
			finished, len(r.trackers), name, pct, extra, fmtBytes(sent), fmtBytes(total), overall, fmtBytes(int64(r.speed)), eta)
	}

	if r.tty {
		pad := ""
		if r.lastLen > len(line) {
			pad = strings.Repeat(" ", r.lastLen-len(line))
		}
		fmt.Fprintf(r.out, "\r%s%s", line, pad)
		r.lastLen = len(line)
	} else {
		fmt.Fprintln(r.out, line)
	}
}

func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-(n-3):]
}

func fmtBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func fmtDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h, m, s := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}