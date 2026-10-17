package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Usage struct {
	Time       string         `json:"time"`
	Account    string         `json:"account"`
	Model      string         `json:"model"`
	Stream     bool           `json:"stream"`
	Success    bool           `json:"success"`
	Error      string         `json:"error,omitempty"`
	DurationMS int64          `json:"duration_ms"`
	Attempts   int            `json:"attempts"`
	Tokens     map[string]any `json:"tokens,omitempty"`
}

func (e *Engine) record(u Usage) {
	e.usageMu.Lock()
	e.usage = append(e.usage, u)
	if len(e.usage) > 500 {
		e.usage = append([]Usage(nil), e.usage[len(e.usage)-500:]...)
	}
	e.usageMu.Unlock()
	e.mu.Lock()
	sink := e.usageSink
	e.mu.Unlock()
	if sink != nil {
		sink(u)
	}
	path := e.settings().UsageFile
	if path == "" {
		return
	}
	e.journalMu.Lock()
	defer e.journalMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return
	}
	if info, err := os.Stat(path); err == nil && info.Size() > 20<<20 {
		if err = os.Rename(path, path+"."+time.Now().Format("20060102-150405.000000000")); err != nil {
			return
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintln(f, string(JSON(u)))
}
func (e *Engine) usageSnapshot() []Usage {
	e.usageMu.Lock()
	defer e.usageMu.Unlock()
	out := make([]Usage, len(e.usage))
	for i, u := range e.usage {
		out[len(out)-1-i] = u
	}
	return out
}
