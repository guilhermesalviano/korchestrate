package artifact

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// LogFile is the append-only audit timeline kept in every run directory.
const LogFile = "run.log.jsonl"

// Log levels used by LogEntry.Level.
const (
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// maxLogMessage bounds one entry so a single huge agent error cannot bloat the
// timeline. The full message is still kept in run.json's error history.
const maxLogMessage = 4096

// LogEntry is one line of a run's audit timeline: a timestamped, structured
// record of what the orchestrator did. Agent output itself lives in the
// per-stage *.events*.jsonl artifacts; this is the orchestrator's narration.
type LogEntry struct {
	Time       time.Time `json:"time"`
	Level      string    `json:"level"`
	Stage      string    `json:"stage,omitempty"`
	Agent      string    `json:"agent,omitempty"`
	Model      string    `json:"model,omitempty"`
	Iter       int       `json:"iter,omitempty"`
	Event      string    `json:"event,omitempty"`
	Message    string    `json:"message"`
	DurationMS int64     `json:"duration_ms,omitempty"`
	ExitCode   *int      `json:"exit_code,omitempty"`
	Usage      *Usage    `json:"usage,omitempty"`
}

// logMu serializes appends so timeline entries never interleave, even when a
// dashboard publish and a pipeline write race (both are rare but possible).
var logMu sync.Mutex

// Log appends one entry to the run's timeline, best-effort: a run without a
// directory, or a write that fails, is not an error worth stopping a run for.
func (r *Run) Log(e LogEntry) error {
	if r == nil || r.Dir == "" {
		return nil
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	e.Message = capLogMessage(e.Message)
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	logMu.Lock()
	defer logMu.Unlock()
	f, err := os.OpenFile(filepath.Join(r.Dir, LogFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}

// ReadLog returns a run's timeline, oldest first. A run from before the log
// existed has none; malformed lines are skipped so one bad write cannot hide
// the rest of the audit trail.
func ReadLog(dir string) ([]LogEntry, error) {
	data, err := os.ReadFile(filepath.Join(dir, LogFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []LogEntry
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e LogEntry
		if json.Unmarshal([]byte(line), &e) == nil {
			out = append(out, e)
		}
	}
	return out, nil
}

func capLogMessage(s string) string {
	if len(s) <= maxLogMessage {
		return s
	}
	return strings.ToValidUTF8(s[:maxLogMessage], "") + " …[truncated]"
}
