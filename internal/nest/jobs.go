package nest

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// A Job is a long piece of work (downloads, installs, backups) that runs in the daemon while the
// CLI or GUI watch its progress.
type Job struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Server   string    `json:"server,omitempty"`
	Step     string    `json:"step"`
	Done     int64     `json:"done"`
	Total    int64     `json:"total"`
	Finished bool      `json:"finished"`
	Error    string    `json:"error,omitempty"`
	Result   any       `json:"result,omitempty"`
	Started  time.Time `json:"started"`

	mu     sync.Mutex
	cancel context.CancelFunc
}

type jobView struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Server   string `json:"server,omitempty"`
	Step     string `json:"step"`
	Done     int64  `json:"done"`
	Total    int64  `json:"total"`
	Finished bool   `json:"finished"`
	Error    string `json:"error,omitempty"`
	Result   any    `json:"result,omitempty"`
}

func (j *Job) view() jobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	return jobView{j.ID, j.Title, j.Server, j.Step, j.Done, j.Total, j.Finished, j.Error, j.Result}
}

// Progress says what the job is doing now; total 0 means it doesn't know how much is left.
func (j *Job) Progress(step string, done, total int64) {
	if j == nil {
		return
	}
	j.mu.Lock()
	j.Step, j.Done, j.Total = step, done, total
	j.mu.Unlock()
}

// Say changes the step without a bar.
func (j *Job) Say(format string, a ...any) { j.Progress(fmt.Sprintf(format, a...), 0, 0) }

type jobList struct {
	mu   sync.Mutex
	jobs map[string]*Job
	n    atomic.Int64
}

func (l *jobList) start(title, server string, run func(ctx context.Context, j *Job) (any, error)) *Job {
	ctx, cancel := context.WithCancel(context.Background())
	j := &Job{ID: fmt.Sprintf("j%d", l.n.Add(1)), Title: title, Server: server, Step: "Starting", Started: time.Now(), cancel: cancel}
	l.mu.Lock()
	if l.jobs == nil {
		l.jobs = map[string]*Job{}
	}
	// Forget finished jobs older than an hour.
	for id, old := range l.jobs {
		if old.Finished && time.Since(old.Started) > time.Hour {
			delete(l.jobs, id)
		}
	}
	l.jobs[j.ID] = j
	l.mu.Unlock()
	go func() {
		defer cancel()
		res, err := func() (res any, err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("something went wrong inside MineNest: %v", r)
				}
			}()
			return run(ctx, j)
		}()
		j.mu.Lock()
		j.Finished = true
		j.Result = res
		if err != nil {
			if ctx.Err() != nil {
				j.Error = "Cancelled."
			} else {
				j.Error = err.Error()
			}
		} else {
			j.Step = "Done"
		}
		j.mu.Unlock()
	}()
	return j
}

func (l *jobList) get(id string) *Job {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.jobs[id]
}

func (l *jobList) list() []jobView {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []jobView{}
	for _, j := range l.jobs {
		if !j.Finished || time.Since(j.Started) < 2*time.Minute {
			out = append(out, j.view())
		}
	}
	return out
}
